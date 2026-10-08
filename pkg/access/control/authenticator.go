package control

import (
	"crypto/subtle"
	"fmt"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	authCacheTTL             = 30 * time.Second
	authCacheMaxSize         = 10000
	authCacheCleanupInterval = 60 * time.Second
)

// Authenticator validates raw API keys with caching.
//
// The principal cache is an EventCache: primary invalidation flows from
// keys.KeyStore.OnChange so a revoked or rotated key stops authenticating
// on the next request rather than lingering until its TTL window expires.
// cacheTTL is retained solely as a memory bound — expired entries are
// evicted by a background cleanup goroutine to cap cache size under load —
// and never serves as a freshness signal.
type Authenticator struct {
	staticKeys StaticKeySet
	keys       keys.KeyStore

	cacheMu  sync.RWMutex
	cache    map[[32]byte]*cachedPrincipal
	cacheTTL time.Duration
	stopCh   chan struct{}
	stopped  sync.Once
	// wg tracks the cleanupLoop goroutine so Stop blocks until it
	// exits — required for clean teardown ordering with Core.Stop
	// and for goleak-based lifecycle tests.
	wg sync.WaitGroup
}

type cachedPrincipal struct {
	principal *KeyPrincipal
	expiresAt time.Time
}

// NewAuthenticator wires the authenticator and subscribes to KeyStore
// mutations so cache entries drop immediately on Create/Update/Delete/
// RotateKey/Reload.
func NewAuthenticator(staticKeys StaticKeySet, keyStore keys.KeyStore) *Authenticator {
	a := &Authenticator{
		staticKeys: staticKeys,
		keys:       keyStore,
		cache:      make(map[[32]byte]*cachedPrincipal),
		cacheTTL:   authCacheTTL,
		stopCh:     make(chan struct{}),
	}
	// A revoked key that was previously cached would stay valid until
	// cacheTTL elapsed without this subscription — unacceptable for key
	// revocation.
	if keyStore != nil {
		keyStore.OnChange(a.Invalidate)
	}
	return a
}

// Invalidate drops every cached principal. Called by the KeyStore on any
// mutation and safe to call at any time.
func (a *Authenticator) Invalidate() {
	a.cacheMu.Lock()
	a.cache = make(map[[32]byte]*cachedPrincipal)
	a.cacheMu.Unlock()
}

// Start launches the background cache cleanup goroutine.
func (a *Authenticator) Start() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.cleanupLoop()
	}()
}

// Stop terminates the cleanup goroutine and blocks until it exits.
// Idempotent.
func (a *Authenticator) Stop() {
	a.stopped.Do(func() {
		close(a.stopCh)
	})
	a.wg.Wait()
}

// Validate resolves a raw API key to a KeyPrincipal. Tries cache → static
// keys → virtual keys (Argon2id). Returns an error when the key is empty,
// invalid, expired, or suspended.
func (a *Authenticator) Validate(rawKey string) (*KeyPrincipal, error) {
	if rawKey == "" {
		return nil, ErrEmptyKey
	}

	// Fast path: cache lookup.
	fp := keys.SHA256Fingerprint(rawKey)
	if p := a.getCached(fp); p != nil {
		return p, nil
	}

	// Static keys (constant-time comparison — fast).
	if p := a.tryStaticKey(rawKey); p != nil {
		a.putCached(fp, p)
		return p, nil
	}

	// Virtual keys (Argon2id — slow, hence the cache).
	if a.keys != nil {
		id, vk := a.keys.ValidateRawKey(rawKey)
		if vk != nil {
			if vk.IsExpired() {
				return nil, fmt.Errorf("%w: %q", ErrKeyExpired, id)
			}
			if vk.Suspended {
				return nil, fmt.Errorf("%w: %q", ErrKeySuspended, id)
			}
			p := keyPrincipalFromVirtual(id, vk, rawKey)
			a.putCached(fp, p)
			return p, nil
		}
	}

	return nil, ErrInvalidKey
}

// tryStaticKey checks the raw key against admin + user. Returns a
// KeyPrincipal on match, nil otherwise.
func (a *Authenticator) tryStaticKey(rawKey string) *KeyPrincipal {
	// Admin.
	if a.staticKeys.Admin != "" &&
		subtle.ConstantTimeCompare([]byte(rawKey), []byte(a.staticKeys.Admin)) == 1 {
		return &KeyPrincipal{
			ID:          "admin",
			Role:        RoleAdmin,
			Name:        "Admin Key",
			Fingerprint: KeyFingerprint(rawKey),
		}
	}

	// User.
	if a.staticKeys.User != "" &&
		subtle.ConstantTimeCompare([]byte(rawKey), []byte(a.staticKeys.User)) == 1 {
		return &KeyPrincipal{
			ID:          "user",
			Role:        RoleUser,
			Name:        "User Key",
			Fingerprint: KeyFingerprint(rawKey),
		}
	}

	return nil
}

// keyPrincipalFromVirtual converts a virtual key record to a KeyPrincipal.
func keyPrincipalFromVirtual(id string, vk *keys.VirtualKey, rawKey string) *KeyPrincipal {
	return &KeyPrincipal{
		ID:          id,
		Name:        vk.Name,
		Fingerprint: KeyFingerprint(rawKey),
		Role:        UserRole(vk.Role),
		IsVirtual:   true,
		TeamID:      vk.TeamID,
		TeamRole:    vk.TeamRole,
		Metadata:    vk.Metadata,
		Quotas:      vk.QuotaConfig,
		Suspended:   vk.Suspended,
		SuspendedAt: vk.SuspendedAt,
		SuspendedBy: vk.SuspendedBy,
		ExpiresAt:   vk.ExpiresAt,
	}
}

func (a *Authenticator) getCached(fp [32]byte) *KeyPrincipal {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()

	entry, ok := a.cache[fp]
	if !ok || utils.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.principal
}

func (a *Authenticator) putCached(fp [32]byte, p *KeyPrincipal) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	if len(a.cache) >= authCacheMaxSize {
		a.evictExpiredLocked()
	}
	if len(a.cache) >= authCacheMaxSize {
		a.evictRandomLocked(len(a.cache) / 4)
	}

	a.cache[fp] = &cachedPrincipal{
		principal: p,
		expiresAt: utils.Now().Add(a.cacheTTL),
	}
}

// InvalidateAll clears the cache. Called after key rotation, deletion, or
// bulk mutations (distinct from the per-entry OnChange invalidation —
// InvalidateAll is the blunter hammer for operator-triggered resets).
func (a *Authenticator) InvalidateAll() {
	a.cacheMu.Lock()
	a.cache = make(map[[32]byte]*cachedPrincipal)
	a.cacheMu.Unlock()
}

func (a *Authenticator) evictExpiredLocked() {
	now := utils.Now()
	for fp, entry := range a.cache {
		if now.After(entry.expiresAt) {
			delete(a.cache, fp)
		}
	}
}

func (a *Authenticator) evictRandomLocked(n int) {
	removed := 0
	for fp := range a.cache {
		if removed >= n {
			break
		}
		delete(a.cache, fp)
		removed++
	}
}

func (a *Authenticator) cleanupLoop() {
	ticker := time.NewTicker(authCacheCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.cacheMu.Lock()
			a.evictExpiredLocked()
			a.cacheMu.Unlock()
		case <-a.stopCh:
			return
		}
	}
}
