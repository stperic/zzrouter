package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type deadlineBody struct {
	ctx context.Context
	io.Reader
	delay time.Duration
}

func (b deadlineBody) Read(p []byte) (int, error) {
	select {
	case <-time.After(b.delay):
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	n, err := b.Reader.Read(p)
	if err == io.EOF && b.delay == 0 {
		<-b.ctx.Done()
		return n, b.ctx.Err()
	}
	return n, err
}
func (deadlineBody) Close() error { return nil }

type deadlineLogHook func(llm.InferenceLogData)

func (f deadlineLogHook) OnInferenceComplete(d llm.InferenceLogData) { f(d) }

func TestInstanceFirstByteDeadline(t *testing.T) {
	for _, preamble := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "preamble"}[preamble], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, inst := instanceStreamTestServer()
				c, w := streamEndContext(t, openai.New())
				ctx, cancel := context.WithTimeout(c.Request.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				ctx = context.WithValue(ctx, CtxKeyOriginalBody, []byte(`{"model":"m","stream":true}`))
				c.Request = c.Request.WithContext(ctx)
				var completion []llm.InferenceLogData
				llm.SetInferenceLogHook(deadlineLogHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
				defer llm.SetInferenceLogHook(nil)
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
					<-req.Context().Done()
					assert.ErrorIs(t, context.Cause(req.Context()), instance.ErrFirstByteTimeout)
					return nil, req.Context().Err()
				})}
				if preamble {
					openEventStream(w)
					s.streamFromInstance(w, c.Request, inst, openai.New())
				} else {
					s.proxyToInstance(w, c.Request, inst, true)
				}
				if !preamble {
					assert.Equal(t, http.StatusGatewayTimeout, w.Code)
				}
				assert.Contains(t, w.Body.String(), `"code":"upstream_timeout"`)
				require.Len(t, completion, 1)
				assert.Equal(t, "upstream_timeout", completion[0].ErrorType)
				assertLeaseReturned(t, inst)
			})
		})
	}
}

func TestInstanceStreamIdleDeadline(t *testing.T) {
	for _, preamble := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "preamble"}[preamble], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, inst := instanceStreamTestServer()
				c, w := streamEndContext(t, openai.New())
				ctx, cancel := context.WithTimeout(c.Request.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				c.Request = c.Request.WithContext(context.WithValue(ctx, CtxKeyOriginalBody, []byte(`{"model":"m","stream":true}`)))
				var completion []llm.InferenceLogData
				llm.SetInferenceLogHook(deadlineLogHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
				defer llm.SetInferenceLogHook(nil)
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: deadlineBody{ctx: req.Context(), Reader: strings.NewReader(partialChatStream)}}, nil
				})}
				if preamble {
					openEventStream(w)
					s.streamFromInstance(w, c.Request, inst, openai.New())
				} else {
					s.proxyToInstance(w, c.Request, inst, true)
				}
				assert.Contains(t, w.Body.String(), `"code":"upstream_stream_interrupted"`)
				assert.NotContains(t, w.Body.String(), "[DONE]")
				require.Len(t, completion, 1)
				assert.Equal(t, "upstream_stream_error", completion[0].ErrorType)
				assert.Equal(t, instance.ErrStreamIdle.Error(), completion[0].ErrorMessage)
				assertLeaseReturned(t, inst)
			})
		})
	}
}

func TestNonStreamHasNoStreamDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, inst := instanceStreamTestServer()
		c, w := streamEndContext(t, openai.New())
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), CtxKeyOriginalBody, []byte(`{"model":"m","stream":false}`)))
		const response = `{"model":"m","choices":[{"message":{"content":"slow but complete"}}]}`
		s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
			select {
			case <-time.After(2 * constants.DefaultFirstByteTimeout):
			case <-req.Context().Done():
				return nil, context.Cause(req.Context())
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: deadlineBody{ctx: req.Context(), Reader: strings.NewReader(response), delay: 2 * constants.DefaultStreamIdleTimeout}}, nil
		})}
		s.proxyToInstance(w, c.Request, inst, true)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "slow but complete")
		assertLeaseReturned(t, inst)
	})
}
