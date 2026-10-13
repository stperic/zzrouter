package gpu

import "context"

// inventoryCacheKey scopes the cached Inventory attached to a
// request-scoped context.Context. Using an unexported struct type
// as the key prevents any cross-package collisions.
type inventoryCacheKey struct{}

// WithCachedInventory shares one hardware view across probes without repeating subprocesses.
func WithCachedInventory(ctx context.Context, snapshot ...Inventory) context.Context {
	if _, ok := cachedInventory(ctx); ok {
		return ctx
	}
	var inv Inventory
	if len(snapshot) > 0 {
		inv = snapshot[0]
	} else {
		inv = InventoryContext(ctx)
	}
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
