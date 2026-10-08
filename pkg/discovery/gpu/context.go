package gpu

import "context"

// inventoryCacheKey scopes the cached Inventory attached to a
// request-scoped context.Context. Using an unexported struct type
// as the key prevents any cross-package collisions.
type inventoryCacheKey struct{}

// WithCachedInventory runs InventoryContext once and attaches the
// result to a derived context. Subsequent calls to InventoryContext
// or ProbeContext that receive this context return cached data
// instead of re-running nvidia-smi / rocm-smi / ghw.
//
// Intended use: wrap the ctx at the entry of any multi-step flow
// that would otherwise probe the same hardware twice — typically
// "run preflight, then build install plan" in the provider
// installer path. A single process-wide TTL cache would violate
// the "safe to call repeatedly so a driver install between calls
// is reflected" contract that runtime callers depend on; ctx-
// scoped caching only applies where the caller explicitly opts in.
func WithCachedInventory(ctx context.Context) context.Context {
	if _, ok := cachedInventory(ctx); ok {
		return ctx
	}
	inv := InventoryContext(ctx)
	return context.WithValue(ctx, inventoryCacheKey{}, &inv)
}

// cachedInventory returns the Inventory attached to ctx by
// WithCachedInventory, if any. Callers consult this before
// invoking any subprocess to keep a single request's hardware
// view coherent.
func cachedInventory(ctx context.Context) (*Inventory, bool) {
	if ctx == nil {
		return nil, false
	}
	inv, ok := ctx.Value(inventoryCacheKey{}).(*Inventory)
	return inv, ok && inv != nil
}

// detectionFromCache extracts a single-vendor Detection from a
// ctx-cached Inventory, reporting whether the cache supplied the
// answer. Used by ProbeContext as a fast path; falls through to
// the real probe when the cache is absent.
func detectionFromCache(ctx context.Context, vendor Vendor) (Detection, bool) {
	inv, ok := cachedInventory(ctx)
	if !ok {
		return Detection{}, false
	}
	det, present := inv.Vendors[vendor]
	if !present {
		return Detection{Vendor: vendor, State: StateAbsent}, true
	}
	return det, true
}
