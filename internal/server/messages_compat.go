package server

import (
	"bytes"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/protocol/anthropic"
)

// messagesCompatEndpoint is the wire_endpoints value of a provider whose
// engine speaks Chat Completions only: zzRouter translates the Messages
// API for it. See docs/plan_messages_compat.md.
const messagesCompatEndpoint = "messages_compat"

// chatCompletionsPath is where a translated Messages request goes.
const chatCompletionsPath = "/v1/chat/completions"

// messagesTranslation is how one Messages route reads as Chat
// Completions: the request it becomes, and how a buffered chat reply
// answers it. A streamed request's reply is translated by
// anthropic.ChatStream.
type messagesTranslation struct {
	request func(body []byte) (anthropic.ChatRequest, error)
	reply   func(chatBody []byte, model string) ([]byte, error)
}

var (
	messagesAsChat    = messagesTranslation{request: anthropic.ToChatRequest, reply: anthropic.FromChatReply}
	countTokensAsChat = messagesTranslation{request: anthropic.ToCountTokensRequest, reply: anthropic.FromChatTokenCount}
)

// dispatchTranslated serves a Messages request on a messages_compat
// target. It becomes a chat request on the chat path, so admission,
// metering, request defaults, fallback and spend are the chat path's,
// and the reply is translated back as it is written. A worker or group
// replica it is proxied to is sent chat; the node that took the client's
// request is the one that translates. The Anthropic responder stays
// attached, so every error zzRouter or the engine raises is already in
// the client's dialect.
func (s *Server) dispatchTranslated(c *gin.Context, resolved *resolver.Resolved, model string, body []byte, tr messagesTranslation) {
	chat, err := tr.request(body)
	if err != nil {
		param := ""
		if terr, ok := err.(*anthropic.TranslateError); ok {
			param = terr.Param
		}
		httperr.FromContext(c).BadRequest(c, err.Error(), param)
		return
	}

	defer asChatRequest(c, chat.Body)()

	// Answer in the client's spelling, a "@node" hint included.
	answerAs := model
	if spelled := clientModelFromContext(c.Request.Context()); spelled != "" {
		answerAs = spelled
	}
	w := newMessagesCompatWriter(c.Writer, answerAs, chat.Stream, tr.reply)
	c.Writer = w
	// Restored even if dispatch panics, so recovery writes to the client
	// and not into the translation buffer.
	defer func() { c.Writer = w.ResponseWriter }()
	s.dispatchTo(c, resolved, model, chat.Body, "chat")
	w.finish(c)
}

// asChatRequest makes the request a Chat Completions request with body,
// for a translation shim to run the chat path, and returns what undoes
// the path: access logs and audit still see the route the client called.
func asChatRequest(c *gin.Context, body []byte) (restore func()) {
	originalPath := c.Request.URL.Path
	c.Request.URL.Path = chatCompletionsPath
	replaceRequestBody(c, body)
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return func() { c.Request.URL.Path = originalPath }
}

// messagesCompatWriter turns the chat reply the dispatch writes into the
// Messages reply the client asked for: streamed frame by frame, or
// buffered and translated whole. A failure is buffered too and leaves in
// the Anthropic dialect: zzRouter's own errors already are, and an
// engine's (OpenAI-shaped) is normalized.
type messagesCompatWriter struct {
	gin.ResponseWriter
	model  string
	reply  func([]byte, string) ([]byte, error)
	stream *anthropic.ChatStream
	buf    bytes.Buffer
	status int
}

func newMessagesCompatWriter(w gin.ResponseWriter, model string, stream bool, reply func([]byte, string) ([]byte, error)) *messagesCompatWriter {
	cw := &messagesCompatWriter{ResponseWriter: w, model: model, reply: reply}
	if stream {
		cw.stream = anthropic.NewChatStream(flushingWriter{w}, model)
	}
	return cw
}

// streaming reports whether the reply goes out as it arrives: a
// successful streamed one. Everything else waits for finish.
func (w *messagesCompatWriter) streaming() bool {
	return w.stream != nil && w.status >= http.StatusOK && w.status < http.StatusMultipleChoices
}

func (w *messagesCompatWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	if w.streaming() {
		// The engine's length is the chat stream's, not this one's.
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "text/event-stream")
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *messagesCompatWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.streaming() {
		w.ResponseWriter.WriteHeaderNow()
	}
}

func (w *messagesCompatWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.streaming() {
		return w.stream.Write(p)
	}
	return w.buf.Write(p)
}

func (w *messagesCompatWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *messagesCompatWriter) Flush() {
	if w.streaming() {
		w.ResponseWriter.Flush()
	}
}

func (w *messagesCompatWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *messagesCompatWriter) Written() bool { return w.status != 0 }

// finish ends the reply: the stream's closing events, or the buffered
// body, translated when it succeeded and in the Anthropic dialect when it
// did not.
func (w *messagesCompatWriter) finish(c *gin.Context) {
	switch {
	case w.status == 0:
		return
	case w.streaming():
		_ = w.stream.Close()
		w.ResponseWriter.Flush()
		return
	case w.status >= http.StatusMultipleChoices:
		body := w.buf.Bytes()
		if !anthropic.IsErrorEnvelope(body) {
			body = anthropic.NormalizeError(w.status, body)
		}
		w.writeWhole(w.status, body)
		return
	}
	out, err := w.reply(w.buf.Bytes(), w.model)
	if err != nil {
		// The engine's length describes the body that failed, not this one.
		w.Header().Del("Content-Length")
		c.Writer = w.ResponseWriter
		httperr.FromContext(c).BadGateway(c, "engine reply could not be translated: "+err.Error())
		return
	}
	w.writeWhole(w.status, out)
}

// writeWhole writes a buffered body as the complete response, replacing
// the engine's length and type.
func (w *messagesCompatWriter) writeWhole(status int, body []byte) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(body)
}

// flushingWriter flushes after every write, so each translated event
// reaches the client as it is made.
type flushingWriter struct{ w gin.ResponseWriter }

func (f flushingWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.w.Flush()
	return n, err
}
