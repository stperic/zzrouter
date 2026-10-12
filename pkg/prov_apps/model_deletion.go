package prov_apps

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// catalogReaders allows parallel observations and one exclusive physical mutation.
const catalogReaders int64 = 1 << 30

func (m *ProviderAppManager) catalogPermit(ctx context.Context, slots int64) (context.Context, func(), error) {
	m.mu.Lock()
	if m.stopping.Load() || m.shutdown.Load() {
		m.mu.Unlock()
		return ctx, nil, ErrShutdown
	}
	m.onDemandStarts.Add(1)
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	end := context.AfterFunc(m.startsCtx, cancel)
	finish := func() { end(); cancel(); m.onDemandStarts.Done() }
	if err := m.catalog.Acquire(ctx, slots); err != nil {
		finish()
		return ctx, nil, err
	}
	return ctx, func() { m.catalog.Release(slots); finish() }, nil
}

// WithModelDeletion excludes physical users until deletion and publication finish.
func (m *ProviderAppManager) WithModelDeletion(ctx context.Context, remove func(context.Context) error) error {
	ctx, release, err := m.catalogPermit(ctx, catalogReaders)
	if err != nil {
		return err
	}
	defer release()
	return remove(ctx)
}

func pathCovers(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	parent = layout.PhysicalPath(parent)
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameWeights(a, b instance.Weights) bool {
	if a.Backend != "" || b.Backend != "" {
		return strings.EqualFold(a.Model, b.Model) && backend.SameEndpoint(a.Backend, b.Backend)
	}
	for _, deleted := range a.Paths {
		for _, held := range b.Paths {
			if pathCovers(deleted, held) || pathCovers(held, deleted) {
				return true
			}
		}
	}
	return (len(a.Paths) == 0 || len(b.Paths) == 0) && strings.EqualFold(a.Model, b.Model)
}

// CheckModelDeletion refuses held weights and configured references under deletion ownership.
func (m *ProviderAppManager) CheckModelDeletion(ctx context.Context, target instance.Weights) error {
	for _, run := range m.instances.List() {
		held := run.Weights()
		if held.Model == "" {
			held.Model = run.Model // legacy/manual registrations have no physical reservation
		}
		if sameWeights(target, held) {
			return httperr.Error{Status: http.StatusConflict, Code: string(httperr.CodeModelInUse), Message: fmt.Sprintf("model weights are held by run %s (%s); stop it through DELETE /zzrouter/v1/runs/%s first", run.ID, run.Model, run.ID), Extra: map[string]any{"run_id": run.ID}}
		}
	}
	var refusal error
	m.AppsConfig().RangeApps(func(provider string, svc config.ServiceConfig) bool {
		for variant, base := range svc.Variants() {
			if ctx.Err() != nil {
				refusal = ctx.Err()
				return false
			}
			base, _, _ = strings.Cut(base, "#")
			weights := instance.Weights{Model: m.CanonicalModelName(base)}
			if target.Backend != "" {
				if _, endpoint, ok := m.daemonOf(provider); ok {
					weights.Backend = endpoint.Endpoint
					weights.Model = m.daemonName(provider, svc, weights.Model)
				}
			} else {
				selection := process.ResolveModel(svc, variant)
				weights.Paths = selection.WeightPaths()
			}
			if sameWeights(target, weights) {
				refusal = httperr.Error{Status: http.StatusConflict, Code: string(httperr.CodeModelReferenced), Message: fmt.Sprintf("model is referenced by variant %s on provider %s; remove models.%s through PATCH /zzrouter/v1/providers/%s/parameters first", variant, provider, variant, provider)}
				return false
			}
		}
		return true
	})
	return refusal
}

func (m *ProviderAppManager) reserveWeights(ctx context.Context, req EnsureRequest, selection launchSelection) instance.Weights {
	name, _, _ := strings.Cut(selection.config.WeightsOf(req.Model), "#")
	weights := instance.Weights{Model: m.CanonicalModelName(name), Paths: selection.model.WeightPaths()}
	if m.modelSource != nil {
		weights.Source = m.modelSource(ctx, name)
	}
	return weights
}
