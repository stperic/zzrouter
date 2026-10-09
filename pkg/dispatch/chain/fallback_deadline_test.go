package chain

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
	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

const chainDeadlineFrame = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"

type chainStallBody struct {
	ctx context.Context
	io.Reader
}

func (b chainStallBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		<-b.ctx.Done()
		return n, b.ctx.Err()
	}
	return n, err
}
func (chainStallBody) Close() error { return nil }

func TestChainLocalStreamUsesLeaseDeadline(t *testing.T) {
	for _, firstByte := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "first_byte"}[firstByte], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deps := newFakeDeps()
				inst := localGroupInstance(t, deps, 0)
				c, w := buildGinCtx(t, `{"model":"group","stream":true}`)
				ctx, cancel := context.WithTimeout(c.Request.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				dep := pfallback.Candidate{Name: "local", App: "engine", Model: "m", OnDemand: true}
				deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
					if firstByte {
						<-deps.forwardedContext.Done()
						return nil, deps.forwardedContext.Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: chainStallBody{deps.forwardedContext, strings.NewReader(chainDeadlineFrame)}}, nil
				}
				p := newProxyWithFake(deps)
				result := p.tryOnce(c, dep, []byte(`{"model":"group","stream":true}`), nil, leaseGroup([]pfallback.Candidate{dep}), routeMetaCtx{decisionStart: utils.Now()})
				if firstByte {
					assert.ErrorIs(t, result.attempt.Err, instance.ErrFirstByteTimeout)
					assert.Equal(t, http.StatusGatewayTimeout, result.attempt.StatusCode)
					assert.Equal(t, attemptRetry, result.disposition)
				} else {
					assert.Contains(t, w.Body.String(), `"code":"upstream_stream_interrupted"`)
					assert.NotContains(t, w.Body.String(), "[DONE]")
				}
				require.NoError(t, ctx.Err(), "the lease deadline must beat the caller safety deadline")
				require.Zero(t, inst.GetActiveRequests())
				next, err := inst.Acquire(context.Background(), nil)
				require.NoError(t, err)
				next.Release()
			})
		})
	}
}

type chainDeadlineHook func(llm.InferenceLogData)

func (f chainDeadlineHook) OnInferenceComplete(d llm.InferenceLogData) { f(d) }

func TestChainFirstByteTimeoutRetriesWithoutCompletingRecorder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deps := newFakeDeps()
		inst := localGroupInstance(t, deps, 0)
		c, w := buildGinCtx(t, `{"model":"group","stream":true}`)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*constants.DefaultFirstByteTimeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		var completion []llm.InferenceLogData
		llm.SetInferenceLogHook(chainDeadlineHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
		defer llm.SetInferenceLogHook(nil)
		rec := llm.NewInferenceRecorder(ctx, "group", "engine")
		deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
			if deps.forwardCalls.Load() == 1 {
				<-deps.forwardedContext.Done()
				return nil, deps.forwardedContext.Err()
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(chainDeadlineFrame + "data: [DONE]\n\n"))}, nil
		}
		newProxyWithFake(deps).ProxyWithFallback(c, leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true, MaxRetries: intPtr(1)}}), []byte(`{"model":"group","stream":true}`), rec, "group", false)
		require.EqualValues(t, 2, deps.forwardCalls.Load())
		require.Len(t, completion, 1)
		assert.Empty(t, completion[0].ErrorType)
		assert.Contains(t, w.Body.String(), "data: [DONE]")
		assert.Zero(t, inst.GetActiveRequests())
	})
}

func TestChainFirstByteTimeoutExhaustionAnswers504(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deps := newFakeDeps()
		inst := localGroupInstance(t, deps, 0)
		c, w := buildGinCtx(t, `{"model":"group","stream":true}`)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*constants.DefaultFirstByteTimeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		var completion []llm.InferenceLogData
		llm.SetInferenceLogHook(chainDeadlineHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
		defer llm.SetInferenceLogHook(nil)
		deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
			<-deps.forwardedContext.Done()
			return nil, deps.forwardedContext.Err()
		}
		newProxyWithFake(deps).ProxyWithFallback(c, leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true, MaxRetries: intPtr(1)}}), []byte(`{"model":"group","stream":true}`), llm.NewInferenceRecorder(ctx, "group", "engine"), "group", false)
		assert.Equal(t, http.StatusGatewayTimeout, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"upstream_timeout"`)
		require.Len(t, completion, 1)
		assert.Equal(t, "upstream_timeout", completion[0].ErrorType)
		assert.Zero(t, inst.GetActiveRequests())
	})
}

func TestChainLocalNonStreamHasNoLeaseDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deps := newFakeDeps()
		inst := localGroupInstance(t, deps, 0)
		c, w := buildGinCtx(t, `{"model":"group","stream":false}`)
		deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
			select {
			case <-time.After(2 * constants.DefaultFirstByteTimeout):
				return cannedJSON(http.StatusOK, `{"choices":[],"model":"m"}`), nil
			case <-deps.forwardedContext.Done():
				return nil, deps.forwardedContext.Err()
			}
		}
		newProxyWithFake(deps).ProxyWithFallback(c, leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true}}), []byte(`{"model":"group","stream":false}`), nil, "group", false)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Zero(t, inst.GetActiveRequests())
	})
}
