// Package client provides an HTTP client library for interacting with zzRouter host servers.
//
// The client supports all zzRouter admin API endpoints including:
//   - Model management (list, load, delete)
//   - Model downloads (deploy, status)
//   - Provider discovery
//
// Example usage:
//
//	config := pkgConfig.ClientNodeConfig{
//	    Address: "http://localhost:9090",
//	    APIKey:  "your-api-key",
//	}
//	client := client.NewClient(config)
//
//	// List all models
//	models, err := client.ListModels(client.ModelFilter{Model: "llama*"})
//
//	// Deploy a model
//	_, err = client.Deploy("llama2", "ollama", "", []string{"gpu-1"}, false)
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jclem/sseparser"
	"github.com/stperic/zzrouter/pkg/apipath"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/security"
)

// Client handles HTTP communication with zzrouter hosts
type Client struct {
	name           string // node name (e.g. "macbook-pro")
	baseURL        string
	apiKey         string
	httpClient     *http.Client
	longHTTPClient *http.Client // for long-running ops like provider install
	tlsConfig      *tls.Config  // cached TLS config for streaming clients
}

// NewClient creates a new API client for a zzrouter host
func NewClient(hostConfig pkgConfig.ClientNodeConfig) *Client {
	httpClient := &http.Client{
		Timeout: constants.HTTPLongTimeout,
	}
	// Separate client for install/upgrade/uninstall — a ~1.5 GB Ollama
	// download on the worker can take minutes, far longer than the 120s
	// general-purpose HTTPLongTimeout. Provider lifecycle ops route through
	// this client via makeLongRequest.
	longHTTPClient := &http.Client{
		Timeout: constants.HTTPDownloadTimeout,
	}

	var tlsCfg *tls.Config
	if hostConfig.TLSCACert != "" {
		if cfg, err := security.TLSClientConfig(hostConfig.TLSCACert); err == nil {
			tlsCfg = cfg
			httpClient.Transport = &http.Transport{TLSClientConfig: tlsCfg}
			longHTTPClient.Transport = &http.Transport{TLSClientConfig: tlsCfg}
		}
	}

	return &Client{
		name:           hostConfig.Name,
		baseURL:        hostConfig.Address,
		apiKey:         hostConfig.APIKey,
		httpClient:     httpClient,
		longHTTPClient: longHTTPClient,
		tlsConfig:      tlsCfg,
	}
}

// newStreamingClient returns an *http.Client with no overall timeout but a
// 30-second response-header timeout, reusing the cached TLS configuration.
func (c *Client) newStreamingClient() *http.Client {
	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       c.tlsConfig,
	}
	return &http.Client{Transport: transport}
}

// Name returns the node name of the connected server.
func (c *Client) Name() string {
	return c.name
}

// BaseURL returns the base URL of the connected server.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// DoRequest performs an authenticated HTTP request and returns the raw
// response. The caller's context is honoured for cancellation and
// deadlines. Callers are responsible for closing the body.
//
// Used by subpackages that need to consume response bodies directly
// (e.g., the logs client, which parses SSE frames off the wire).
func (c *Client) DoRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.makeContextRequest(ctx, method, path, body, nil)
}

// DoStreamingRequest performs an authenticated streaming HTTP request
// with no overall timeout. Callers are responsible for closing the body.
// Used by subpackages consuming long-lived SSE streams.
func (c *Client) DoStreamingRequest(ctx context.Context, method, path string) (*http.Response, error) {
	return c.makeStreamingRequest(ctx, method, path, nil, nil)
}

// makeContextRequest is the context-aware form of makeRequest. Used by
// the DoRequest export so subpackages can thread ctx end-to-end.
func (c *Client) makeContextRequest(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, error) {
	req, err := c.newJSONRequest(ctx, method, path, body, headers)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// makeRequest performs an HTTP request with proper authentication.
// Existing callers use context.Background(); new code should prefer
// makeContextRequest for cancellation-aware calls.
func (c *Client) makeRequest(method, path string, body any) (*http.Response, error) {
	return c.makeRequestWithHeaders(method, path, body, nil)
}

// makeLongRequest is makeRequest but uses longHTTPClient, which has a much
// larger overall timeout suitable for provider install/upgrade/uninstall.
// A remote Ollama install can pull a ~1.5 GB tarball which overruns the
// general-purpose HTTPLongTimeout on slow links.
func (c *Client) makeLongRequest(method, path string, body any) (*http.Response, error) {
	req, err := c.newJSONRequest(context.Background(), method, path, body, nil)
	if err != nil {
		return nil, err
	}
	return c.longHTTPClient.Do(req)
}

func (c *Client) makeRequestWithHeaders(method, path string, body any, headers map[string]string) (*http.Response, error) {
	return c.makeContextRequest(context.Background(), method, path, body, headers)
}

// makeStreamingRequest is like makeRequestWithHeaders but uses the streaming
// client (no overall timeout) for long-running SSE connections.
// The context allows the caller to cancel the request (e.g., Ctrl+C in chat).
func (c *Client) makeStreamingRequest(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, error) {
	req, err := c.newJSONRequest(ctx, method, path, body, headers)
	if err != nil {
		return nil, err
	}
	return c.newStreamingClient().Do(req)
}

// newJSONRequest builds an authenticated request whose body, if any, is
// body encoded as JSON.
func (c *Client) newJSONRequest(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Request, error) {
	if body == nil {
		return c.newRequest(ctx, method, path, nil, "", headers)
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	return c.newRequest(ctx, method, path, bytes.NewReader(jsonData), "application/json", headers)
}

// newRequest builds an authenticated request; every request the client
// sends starts here. contentType is set only with a body.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, contentType string, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// doJSON sends method/path with optional body via the regular HTTP client,
// checks status, and decodes the JSON response into target (if non-nil).
// Non-2xx bodies are parsed with parseErrorResponse to produce RFC 9457 /
// OpenAI-aware error text.
func (c *Client) doJSON(method, path string, body, target any, operation string) error {
	return c.doWith(c.makeRequest, method, path, body, target, operation)
}

// doLongJSON is doJSON using longHTTPClient (install/upgrade/download paths
// that can legitimately take minutes).
func (c *Client) doLongJSON(method, path string, body, target any, operation string) error {
	return c.doWith(c.makeLongRequest, method, path, body, target, operation)
}

func (c *Client) doWith(
	send func(method, path string, body any) (*http.Response, error),
	method, path string, body, target any, operation string,
) error {
	resp, err := send(method, path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponse(resp, target, operation)
}

// readResponse checks a response's status and reads its body into target:
// raw bytes for a *[]byte, JSON otherwise, nothing for nil. Non-2xx bodies
// are parsed with parseErrorResponse to produce RFC 9457 / OpenAI-aware
// error text. The caller owns and closes the body.
func readResponse(resp *http.Response, target any, operation string) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return &APIError{
			Operation:  operation,
			StatusCode: resp.StatusCode,
			Message:    parseErrorResponse(errBody),
		}
	}
	switch t := target.(type) {
	case nil:
		return nil
	case *[]byte:
		// A file endpoint answers with the file itself, not JSON.
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("%s: read response: %w", operation, readErr)
		}
		*t = data
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("%s: decode response: %w", operation, err)
	}
	return nil
}

// parseErrorResponse attempts to parse a JSON error response and extract user-friendly message
func parseErrorResponse(body []byte) string {
	msg, _ := parseErrorEnvelope(body)
	return msg
}

// parseErrorEnvelope also returns the OpenAI error `type`. Callers that
// surface the failure to a person want it: the message alone cannot
// distinguish a throttled provider from a broken one.
func parseErrorEnvelope(body []byte) (message, errType string) {
	var errResp ErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil {
		if msg := errResp.ErrorMessage(); msg != "" {
			return msg, errResp.ErrorType()
		}
	}
	// If JSON parsing fails, return the raw body
	return string(body), ""
}

// SearchModelsWithTags searches for models with custom sort and tag parameters (V2)
// GET /zzrouter/search?type=models&q=<query>&provider=<provider>&limit=<limit>&sort=<sort>&order=<order>&tags=<tags>&logic=<logic>
func (c *Client) SearchModelsWithTags(query, provider string, limit int, sort, order, tags, logic string) (map[string]any, error) {
	if provider == "" {
		provider = "all"
	}
	if limit <= 0 {
		limit = 100
	}
	if sort == "" {
		sort = "trendingScore"
	}
	if order == "" {
		order = "desc"
	}
	if logic == "" {
		logic = "AND"
	}

	// URL encode query parameters (include full=true to get siblings/variants, enrich=true for pricing-store metadata)
	path := fmt.Sprintf(apipath.Search+"?type=models&q=%s&provider=%s&limit=%d&sort=%s&order=%s&full=true&enrich=true",
		url.QueryEscape(query), url.QueryEscape(provider), limit, url.QueryEscape(sort), url.QueryEscape(order))

	// Add tags filter if specified
	if tags != "" {
		path += fmt.Sprintf("&tags=%s&logic=%s", url.QueryEscape(tags), url.QueryEscape(logic))
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &envelope, "search"); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// GetSearchProviders fetches the list of search providers and their metadata.
// GET /zzrouter/v1/search?type=providers
func (c *Client) GetSearchProviders() ([]map[string]any, error) {
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := c.doJSON("GET", apipath.Search+"?type=providers", nil, &envelope, "get search providers"); err != nil {
		return nil, err
	}

	resultsRaw, ok := envelope.Data["results"]
	if !ok {
		return nil, fmt.Errorf("no results in response")
	}
	resultsSlice, ok := resultsRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid results format")
	}

	var providers []map[string]any
	for _, item := range resultsSlice {
		if m, ok := item.(map[string]any); ok {
			providers = append(providers, m)
		}
	}
	return providers, nil
}

// GetModelCard retrieves detailed model information including file variants
// GET /zzrouter/models/card/:provider/*id (wildcard route handles slashes)
func (c *Client) GetModelCard(provider, modelID string) (map[string]any, error) {
	// Don't encode slashes - the wildcard route handles them
	path := apipath.ModelCard(provider, modelID)
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &envelope, "get model card"); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// GetInstance retrieves detailed information for a specific instance
// GET /zzrouter/runs/:id (V2)
func (c *Client) GetInstance(instanceID, host string) (*Instance, error) {
	path := apipath.Run(instanceID) + "?node=" + url.QueryEscape(host)
	var envelope struct {
		Data Instance `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &envelope, "get instance"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// GetInstanceLogs retrieves logs for a specific instance (V2)
// GET /zzrouter/runs/:id/logs?lines=N&follow=true/false
// For streaming mode (follow=true), use StreamInstanceLogs instead
func (c *Client) GetInstanceLogs(instanceID, host string, lines int, follow bool) ([]string, error) {
	if follow {
		return nil, fmt.Errorf("for streaming logs, use StreamInstanceLogs() instead")
	}

	params := url.Values{}
	params.Set("node", host)
	if lines > 0 {
		params.Set("lines", fmt.Sprintf("%d", lines))
	}
	path := apipath.RunLogs(instanceID) + "?" + params.Encode()

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Lines []string `json:"lines"`
		} `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &response, "get instance logs"); err != nil {
		return nil, err
	}
	if !response.Success {
		return nil, fmt.Errorf("API returned success=false")
	}
	return response.Data.Lines, nil
}

// StreamInstanceLogs streams logs for a specific instance in real-time (V2)
// GET /zzrouter/runs/:id/logs?follow=true
// Prints logs to stdout as they arrive. Press Ctrl+C to stop.
func (c *Client) StreamInstanceLogs(instanceID, host string) error {
	path := apipath.RunLogs(instanceID) + "?follow=true&node=" + url.QueryEscape(host) // V2 endpoint

	// Use a client with no overall timeout but with a response header timeout
	// so the initial connection can fail fast while allowing long streaming responses
	streamClient := c.newStreamingClient()

	// Build full URL
	fullURL := c.baseURL + path

	// TODO: Thread context from callers for proper cancellation support
	req, err := http.NewRequestWithContext(context.Background(), "GET", fullURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Add API key if configured
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := streamClient.Do(req) //nolint:bodyclose // body close happens via signal handler + deferred signal.Stop block below
	if err != nil {
		return err
	}

	// Handle Ctrl+C signal - close response body to unblock scanner.
	// cancelled distinguishes user-initiated shutdown from a real stream error
	// so we can suppress the post-Close scanner.Err() without string-matching it.
	var cancelled atomic.Bool
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancelled.Store(true)
		_ = resp.Body.Close() // This will cause scanner.Scan() to return false
	}()
	defer func() {
		signal.Stop(sigChan)
		cancelled.Store(true)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream logs failed with status %d: %s", resp.StatusCode, parseErrorResponse(body))
	}

	// Stream logs line by line, parsing SSE format
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// Parse SSE format: skip "event:" lines and extract content from "data:" lines
		if logContent, ok := strings.CutPrefix(line, "data:"); ok {
			// Normalize control characters in log content
			// 1) Convert real carriage returns to newlines so progress-style output
			//    becomes regular multi-line text instead of overwriting in-place.
			logContent = strings.ReplaceAll(logContent, "\r", "\n")
			// 2) Convert literal "\\r" escape sequences to real newlines for consistent display.
			logContent = strings.ReplaceAll(logContent, "\\r", "\n")
			// 3) Trim surrounding whitespace
			logContent = strings.TrimSpace(logContent)
			if logContent == "" {
				continue
			}
			fmt.Println(logContent)
		}
		// Skip "event:" lines and empty lines
	}

	// Don't report error if we closed the body ourselves (user cancelled or deferred cleanup)
	if err := scanner.Err(); err != nil && !cancelled.Load() {
		return fmt.Errorf("error reading log stream: %w", err)
	}

	return nil
}

// GetProviders retrieves the list of enabled providers from the server.
// host filters by host (optional). refresh forces the server to re-probe.
func (c *Client) GetProviders(host string, refresh ...bool) (map[string]any, error) {
	endpoint := apipath.Providers
	sep := "?"
	if host != "" {
		endpoint += fmt.Sprintf("?node=%s", url.QueryEscape(host))
		sep = "&"
	}
	if len(refresh) > 0 && refresh[0] {
		endpoint += sep + "refresh=true"
	}
	var response map[string]any
	if err := c.doJSON("GET", endpoint, nil, &response, "get providers"); err != nil {
		return nil, err
	}
	return response, nil
}

// GetNodesWithRefresh retrieves the list of nodes with optional refresh
// If refresh is true, server will re-probe all cluster workers
func (c *Client) GetNodesWithRefresh(fields string, refresh bool) (map[string]any, error) {
	q := url.Values{}
	if fields != "" {
		q.Set("fields", fields)
	}
	if refresh {
		q.Set("refresh", "true")
	}
	endpoint := apipath.Nodes
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	var response map[string]any
	if err := c.doJSON("GET", endpoint, nil, &response, "get nodes"); err != nil {
		return nil, err
	}
	return response, nil
}

// GetNode retrieves a single node with fresh data (re-probes the worker)
func (c *Client) GetNode(name string) (map[string]any, error) {
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := c.doJSON("GET", apipath.Node(name), nil, &envelope, "get node"); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// ChatMessage represents a message in the conversation
type ChatMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"` // For models that support reasoning (e.g., Hermes 3)
}

// ChatRequest represents the request to /v1/chat/completions.
// Temperature and TopP are pointers so omitempty truly omits them when not set.
// Some providers (Anthropic) reject requests containing both fields.
type ChatRequest struct {
	Model         string        `json:"model"`
	Messages      []ChatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	MaxTokens     int           `json:"max_tokens,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`

	// PreferredNode is not serialized — used to set X-Node header for routing
	PreferredNode string `json:"-"`
}

// StreamChunk represents a chunk of streaming response
type StreamChunk struct {
	Content          string  // Regular response content
	Reasoning        string  // Thinking/reasoning content
	IsThinking       bool    // True if this is a thinking chunk
	IsStatusUpdate   bool    // True if this is a status update (model loading)
	StatusType       string  // Status type: "loading_model", "model_ready", "error"
	Model            string  // Actual model name from the backend response
	Provider         string  // Provider that served the request (from X-zzrouter-Provider header)
	Node             string  // Node that served the request (from X-zzrouter-Node header)
	PromptTokens     int     // Total tokens in the prompt (normalized across providers)
	CompletionTokens int     // Tokens in the completion
	TotalTokens      int     // Total tokens used
	CachedTokens     int     // Prompt tokens served from KV cache (0 if unknown)
	ReasoningTokens  int     // Tokens used for reasoning/thinking (0 if unknown)
	Cost             float64 // Cost in USD (0 if free or unknown)
}

type sseEvent struct {
	Event string       `sse:"event"` // Event type (e.g., "status_update")
	Data  sseChunkData `sse:"data"`
}

// sseChunkData represents the data field of an SSE event
type sseChunkData struct {
	// Model name from the backend response (e.g., "llama3:latest")
	Model string `json:"model,omitempty"`

	// Standard OpenAI streaming fields
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Chain-of-thought arrives under two different names: OpenRouter
			// and friends emit `reasoning`, while llama.cpp, vLLM and
			// DeepSeek emit `reasoning_content`. Decoding only one silently
			// drops every thinking token from the other half of the fleet —
			// which reads as a model that answered with nothing.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		TotalTokens         int     `json:"total_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`

	// Custom status update fields (for model loading progress)
	Status  string `json:"status,omitempty"`  // e.g., "loading_model", "model_ready", "error"
	Message string `json:"message,omitempty"` // Human-readable status message

	// OpenAI-compatible error in SSE data chunk
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// UnmarshalSSEValue implements sseparser.UnmarshalerSSEValue
func (c *sseChunkData) UnmarshalSSEValue(v string) error {
	// Handle stream end marker
	if v == "[DONE]" {
		return nil
	}
	// Skip empty values (can happen with keep-alive events)
	if v == "" || v == " " {
		return nil
	}
	// Unmarshal the JSON data
	return json.Unmarshal([]byte(v), c)
}

// ChatCompletionsStream sends a streaming chat completion request
// POST /v1/chat/completions with stream=true
// Callback is called for each token as it arrives
// Also handles status_update events during model loading
func (c *Client) ChatCompletionsStream(ctx context.Context, req ChatRequest, callback func(chunk StreamChunk) error) error {
	// Force streaming mode and request usage stats
	req.Stream = true
	req.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{
		IncludeUsage: true,
	}

	var extraHeaders map[string]string
	if req.PreferredNode != "" {
		extraHeaders = map[string]string{"X-Node": req.PreferredNode}
	}
	resp, err := c.makeStreamingRequest(ctx, "POST", "/v1/chat/completions", req, extraHeaders)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		msg, errType := parseErrorEnvelope(body)
		return &ChatError{StatusCode: resp.StatusCode, Message: msg, Type: errType}
	}

	// Detect provider and node for usage normalization and display
	provider := resp.Header.Get(constants.HeaderServingProvider)
	servedByNode := resp.Header.Get(constants.HeaderServingNode)

	// Estimate total prompt tokens from request messages (for Ollama normalization)
	estimatedPromptTokens := 0
	if provider == constants.AppOllama {
		for _, msg := range req.Messages {
			estimatedPromptTokens += len(msg.Content) / 4
		}
		// Account for chat template overhead (~10 tokens per message)
		estimatedPromptTokens += len(req.Messages) * 10
	}

	// Use sseparser to read SSE stream
	scanner := sseparser.NewStreamScanner(resp.Body)

	for {
		var event sseEvent
		_, err := scanner.UnmarshalNext(&event)
		if err != nil {
			if errors.Is(err, sseparser.ErrStreamEOF) {
				return nil
			}
			return fmt.Errorf("error reading stream: %w", err)
		}

		// Handle custom status_update events (model loading progress)
		if event.Event == "status_update" {
			if event.Data.Status != "" {
				// Send status update as a special chunk type
				if err := callback(StreamChunk{
					Content:        event.Data.Message,
					IsStatusUpdate: true,
					StatusType:     event.Data.Status,
				}); err != nil {
					return err
				}

				// If status is "error", return error after callback
				if event.Data.Status == "error" {
					return fmt.Errorf("%s", event.Data.Message)
				}
			}
			continue
		}

		// Handle error chunks (OpenAI-compatible error in SSE data).
		// The envelope's `type` is the actionable half — keep it rather
		// than flattening the whole thing to a message string.
		if event.Data.Error != nil {
			return &ChatError{
				StatusCode: resp.StatusCode,
				Message:    event.Data.Error.Message,
				Type:       event.Data.Error.Type,
			}
		}

		// Handle usage stats (sent at end of stream)
		if event.Data.Usage != nil {
			chunk := StreamChunk{
				PromptTokens:     event.Data.Usage.PromptTokens,
				CompletionTokens: event.Data.Usage.CompletionTokens,
				TotalTokens:      event.Data.Usage.TotalTokens,
				Cost:             event.Data.Usage.Cost,
			}

			// Parse prompt_tokens_details if present (vLLM/OpenAI)
			if event.Data.Usage.PromptTokensDetails != nil {
				chunk.CachedTokens = event.Data.Usage.PromptTokensDetails.CachedTokens
			}

			// Parse completion_tokens_details if present (OpenRouter/OpenAI)
			if event.Data.Usage.CompletionTokensDetails != nil {
				chunk.ReasoningTokens = event.Data.Usage.CompletionTokensDetails.ReasoningTokens
			}

			// Normalize Ollama usage: prompt_tokens from Ollama is eval'd only
			if provider == constants.AppOllama && estimatedPromptTokens > 0 {
				evalTokens := chunk.PromptTokens
				totalPrompt := max(estimatedPromptTokens, evalTokens)
				chunk.CachedTokens = totalPrompt - evalTokens
				chunk.PromptTokens = totalPrompt
				chunk.TotalTokens = chunk.PromptTokens + chunk.CompletionTokens
			}

			if err := callback(chunk); err != nil {
				return err
			}
		}

		// Extract content/reasoning and call callback
		if len(event.Data.Choices) > 0 {
			delta := event.Data.Choices[0].Delta

			// Handle reasoning/thinking chunks
			reasoning := delta.Reasoning
			if reasoning == "" {
				reasoning = delta.ReasoningContent
			}
			if reasoning != "" {
				if err := callback(StreamChunk{
					Reasoning:  reasoning,
					IsThinking: true,
					Model:      event.Data.Model,
					Provider:   provider,
					Node:       servedByNode,
				}); err != nil {
					return err
				}
			}

			// Handle regular content chunks
			if delta.Content != "" {
				if err := callback(StreamChunk{
					Content:    delta.Content,
					IsThinking: false,
					Model:      event.Data.Model,
					Provider:   provider,
					Node:       servedByNode,
				}); err != nil {
					return err
				}
			}
		}
	}
}
