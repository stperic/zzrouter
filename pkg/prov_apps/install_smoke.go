package prov_apps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// DefaultInstallSmokeTimeout bounds readiness and one chat when config omits it.
	DefaultInstallSmokeTimeout = 5 * time.Minute
	installSmokeMaxTokens      = 128             // Allows reasoning before final content.
	smokeAdmissionWait         = 5 * time.Second // Bounds retries while another engine occupies resources.
)

var errSmokeBusy = errors.New("smoke could not run: resources busy")
var errSmokeRollbackBlocked = errors.New("smoke process exit unconfirmed: rollback blocked; operator must stop the engine before replacing runtime files")

func (m *ProviderAppManager) smokeInstalledRuntime(ctx context.Context, provider, runtime, model string) (result error) {
	_, svc, ok := m.resolveConfigKey(provider)
	if !ok {
		return ErrProviderNotFound
	}
	timeout := DefaultInstallSmokeTimeout
	if svc.Runtime != nil && svc.Runtime.HealthCheck.ReadinessProbe != nil && svc.Runtime.HealthCheck.ReadinessProbe.Timeout != "" {
		value := svc.Runtime.HealthCheck.ReadinessProbe.Timeout
		var err error
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return fmt.Errorf("invalid provider readiness timeout %q", value)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var run *instance.Instance
	var err, lastBusy error
	retryUntil := utils.Now().Add(smokeAdmissionWait)
	for {
		run, err = m.LaunchInstance(ctx, LaunchRequest{Provider: provider, Runtime: runtime, Model: model, installSmoke: true})
		if lastBusy != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return fmt.Errorf("%w: %v", errSmokeBusy, err)
		}
		var fit *process.MemoryFitError
		if errors.As(err, &fit) {
			return fmt.Errorf("smoke model does not fit this GPU: %w", err)
		}
		if err == nil || !(errors.Is(err, ErrAtCapacity) || errors.Is(err, ErrInstancesRunning)) {
			break
		}
		lastBusy = err
		if !utils.Now().Before(retryUntil) {
			return fmt.Errorf("%w: %v", errSmokeBusy, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", errSmokeBusy, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	defer func() {
		// A timed-out smoke must still stop its own run before rollback.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.resolveShutdownTimeout(provider)+5*time.Second)
		defer cancel()
		if err := m.StopInstance(cleanup, run.ID); err != nil && !errors.Is(err, ErrInstanceNotFound) {
			result = errors.Join(result, fmt.Errorf("%w: %v", errSmokeRollbackBlocked, err))
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if run.GetStatus() == instance.StatusRunning {
			return m.smokeChat(ctx, run)
		}
		if run.GetStatus().IsTerminal() {
			return fmt.Errorf("smoke run ended before readiness (%s)", run.GetStatus())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *ProviderAppManager) smokeChat(ctx context.Context, run *instance.Instance) error {
	body, _ := json.Marshal(map[string]any{"model": run.WireModel, "messages": []map[string]string{{"role": "user", "content": "Say OK."}}, "max_tokens": installSmokeMaxTokens, "stream": false})
	// Fixed string, boolean and integer values are always JSON encodable.
	target := backend.Resolved{Endpoint: fmt.Sprintf("http://127.0.0.1:%d", run.Port), Upstream: backend.Engine()}
	req, err := target.NewRequest(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := *m.httpClient
	client.Timeout = 0 // The provider-configured context owns the smoke deadline.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("smoke chat: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("smoke chat returned HTTP %d", response.StatusCode)
	}
	var reply struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&reply); err != nil {
		return fmt.Errorf("smoke chat response: %w", err)
	}
	if len(reply.Choices) == 0 || (strings.TrimSpace(reply.Choices[0].Message.Content) == "" && strings.TrimSpace(reply.Choices[0].Message.ReasoningContent) == "") {
		return fmt.Errorf("smoke chat returned an empty reply")
	}
	return nil
}
