package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// The in-memory authority tests client ownership failures; shared-authority integration is separate.
type browserAuthorityFixture struct {
	mu                 sync.Mutex
	clients            map[string]bool
	created, retired   int
	retireErr, authErr error
	constrained        bool
}

func (a *browserAuthorityFixture) CreateClient(context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.created++
	id := fmt.Sprintf("owned_%d", a.created)
	a.clients[id] = true
	return id, nil
}

func (a *browserAuthorityFixture) RetireClient(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retireErr != nil {
		return a.retireErr
	}
	delete(a.clients, id)
	a.retired++
	return nil
}

func (a *browserAuthorityFixture) ResetClients(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clients = map[string]bool{}
	return a.retireErr
}

func (a *browserAuthorityFixture) Authenticate(context.Context, string) (*AccessContext, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authErr != nil {
		return nil, a.authErr
	}
	ac := &AccessContext{Key: &KeyPrincipal{ID: "user", Role: RoleUser}}
	if a.constrained {
		ac.Key.IsVirtual = true
		ac.Team = &TeamPrincipal{AllowedModels: []string{"approved"}}
	}
	return ac, nil
}

func browserGatewayFixture(t *testing.T, tokenHandler http.Handler) (*providerBrowserGateway, *browserAuthorityFixture) {
	t.Helper()
	authority := &browserAuthorityFixture{}
	issuer := httptest.NewServer(tokenHandler)
	t.Cleanup(issuer.Close)
	s := &Server{node: &NodeIdentity{name: "local"}}
	gateway, err := newProviderBrowserGateway(t.Context(), s, "https://comfy.example.com", issuer.URL, authority, issuer.Client())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, gateway.close(ctx))
	})
	return gateway, authority
}

func beginBrowserAuthorization(t *testing.T, gateway *providerBrowserGateway) (*url.URL, *http.Cookie) {
	return beginBrowserAuthorizationAt(t, gateway, "/runs/local/run-a/?tab=one")
}

func beginBrowserAuthorizationAt(t *testing.T, gateway *providerBrowserGateway, path string) (*url.URL, *http.Cookie) {
	t.Helper()
	return beginBrowserAuthorizationFrom(t, gateway, path, "192.0.2.1:1234")
}

func beginBrowserAuthorizationFrom(t *testing.T, gateway *providerBrowserGateway, path, peer string) (*url.URL, *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, gateway.origin.String()+path, nil)
	r.RemoteAddr = peer
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	location, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "S256", location.Query().Get("code_challenge_method"))
	require.Equal(t, gateway.issuer+providerBrowserResource, location.Query().Get("resource"))
	return location, w.Result().Cookies()[0]
}

func browserCallback(gateway *providerBrowserGateway, location *url.URL, proof *http.Cookie) *httptest.ResponseRecorder {
	query := url.Values{"state": {location.Query().Get("state")}, "code": {"one-use-code"}, "iss": {gateway.issuer}}
	r := httptest.NewRequest(http.MethodGet, gateway.origin.String()+"/oauth/callback?"+query.Encode(), nil)
	r.AddCookie(proof)
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	return w
}

func successfulBrowserToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"private-access-token","refresh_token":"private-refresh-token","token_type":"Bearer","expires_in":900}`))
}

func TestProviderBrowserOAuthPKCEAndSingleUseCallback(t *testing.T) {
	forms := make(chan url.Values, 1)
	g, authority := browserGatewayFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if r.ParseForm() == nil {
			forms <- r.PostForm
		}
		successfulBrowserToken(w, r)
	}))
	location, proof := beginBrowserAuthorization(t, g)
	bad := *proof
	bad.Value = "wrong-browser"
	w := browserCallback(g, location, &bad)
	require.Equal(t, http.StatusForbidden, w.Code)
	w = browserCallback(g, location, proof)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	require.Equal(t, "/runs/local/run-a/?tab=one", w.Header().Get("Location"))
	form := <-forms
	require.Equal(t, location.Query().Get("code_challenge"), oauth2.S256ChallengeFromVerifier(form.Get("code_verifier")))
	require.Equal(t, location.Query().Get("client_id"), form.Get("client_id"))
	require.Equal(t, g.issuer+providerBrowserResource, form.Get("resource"))
	cookie := w.Result().Cookies()[0]
	require.True(t, cookie.HttpOnly && cookie.Secure)
	require.True(t, strings.HasPrefix(cookie.Name, "__Host-"))
	require.Empty(t, cookie.Domain)
	require.Equal(t, "/", cookie.Path)
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	require.NotContains(t, w.Body.String()+fmt.Sprint(w.Header()), "private-")
	w = browserCallback(g, location, proof)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Zero(t, authority.retired, "a replay cannot retire the accepted session")
	r := httptest.NewRequest(http.MethodPost, g.origin.String()+"/logout", nil)
	r.Header.Set("Origin", g.origin.String())
	r.AddCookie(cookie)
	session := g.session(r)
	require.NotNil(t, session)
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.ErrorIs(t, session.ctx.Err(), context.Canceled)
	require.Empty(t, authority.clients)
}

func TestProviderBrowserFailedExchangeRetiresKnownClient(t *testing.T) {
	for _, dropped := range []bool{false, true} {
		t.Run(fmt.Sprint(dropped), func(t *testing.T) {
			g, authority := browserGatewayFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if dropped {
					conn, _, err := http.NewResponseController(w).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("private-refresh-token must never escape"))
			}))
			location, proof := beginBrowserAuthorization(t, g)
			w := browserCallback(g, location, proof)
			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.NotContains(t, w.Body.String(), "private-refresh-token")
			require.Empty(t, authority.clients)
			require.Empty(t, g.states)
			require.Equal(t, 1, authority.retired)
		})
	}
}

func TestProviderBrowserRetirementFailureRetainsBoundedCapacity(t *testing.T) {
	g, authority := browserGatewayFixture(t, http.HandlerFunc(successfulBrowserToken))
	for index := range providerBrowserMaxSessions {
		_, _ = beginBrowserAuthorizationFrom(t, g, "/runs/local/run-a/", fmt.Sprintf("192.0.2.%d:1234", index+1))
	}
	authority.mu.Lock()
	authority.retireErr = errors.New("durable storage unavailable")
	authority.mu.Unlock()
	g.mu.Lock()
	for _, a := range g.states {
		a.expires = utils.Now().Add(-time.Second)
	}
	g.mu.Unlock()
	g.expire(t.Context())
	require.Len(t, g.states, providerBrowserMaxSessions)
	r := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/local/run-a/", nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, providerBrowserMaxSessions, authority.created)
	authority.mu.Lock()
	authority.retireErr = nil
	authority.mu.Unlock()
	g.expire(t.Context())
	require.Empty(t, g.states)
	require.Empty(t, authority.clients)
}

func TestProviderBrowserPendingAdmissionPreservesOtherPeerSignIn(t *testing.T) {
	g, authority := browserGatewayFixture(t, http.HandlerFunc(successfulBrowserToken))
	for index := range providerBrowserMaxSessions {
		request := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/local/unknown-run/", nil)
		request.RemoteAddr = fmt.Sprintf("192.0.2.1:%d", 1000+index)
		request.Header.Set("Accept", "text/html")
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", index+1))
		result := httptest.NewRecorder()
		g.ServeHTTP(result, request)
		want := http.StatusTooManyRequests
		if index < providerBrowserMaxPendingPerPeer {
			want = http.StatusSeeOther
		}
		require.Equal(t, want, result.Code)
	}
	require.Equal(t, providerBrowserMaxPendingPerPeer, authority.created)
	require.Len(t, g.states, providerBrowserMaxPendingPerPeer)
	location, proof := beginBrowserAuthorizationFrom(t, g, "/runs/local/run-a/", "198.51.100.1:4321")
	result := browserCallback(g, location, proof)
	require.Equal(t, http.StatusSeeOther, result.Code)
	request := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/local/run-a/prompt", nil)
	request.AddCookie(result.Result().Cookies()[0])
	session := g.session(request)
	require.NotNil(t, session)
	for range providerBrowserMaxPendingPerPeer {
		_, _ = beginBrowserAuthorizationFrom(t, g, "/runs/local/run-a/", "198.51.100.1:4321")
	}
	require.Same(t, session, g.session(request), "pending pressure cannot evict an active session")
	require.NoError(t, session.ctx.Err())
}

func TestProviderBrowserPendingPeerUsesOnlyTransportAddress(t *testing.T) {
	for _, test := range []struct{ address, peer string }{
		{"192.0.2.1:1234", "192.0.2.1"},
		{"[::ffff:192.0.2.1]:4321", "192.0.2.1"},
		{"[2001:db8::1]:1234", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"invalid", "unknown"},
	} {
		request := httptest.NewRequest(http.MethodGet, "https://comfy.example.com", nil)
		request.RemoteAddr = test.address
		request.Header.Set("Forwarded", "for=198.51.100.1")
		request.Header.Set("X-Forwarded-For", "198.51.100.1")
		require.Equal(t, test.peer, providerBrowserPeer(request))
	}
}

func TestProviderBrowserRevocationAndNewOpaqueLimitsCloseSession(t *testing.T) {
	for _, constrained := range []bool{false, true} {
		t.Run(fmt.Sprint(constrained), func(t *testing.T) {
			g, authority := browserGatewayFixture(t, http.HandlerFunc(successfulBrowserToken))
			location, proof := beginBrowserAuthorization(t, g)
			w := browserCallback(g, location, proof)
			require.Equal(t, http.StatusSeeOther, w.Code)
			r := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/local/run-a/prompt", nil)
			r.AddCookie(w.Result().Cookies()[0])
			session := g.session(r)
			require.NotNil(t, session)
			authority.mu.Lock()
			if constrained {
				authority.constrained = true
			} else {
				authority.authErr = errors.New("identity revoked")
			}
			authority.mu.Unlock()
			w = httptest.NewRecorder()
			g.ServeHTTP(w, r)
			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.ErrorIs(t, session.ctx.Err(), context.Canceled)
			require.Empty(t, authority.clients)
		})
	}
}

func TestProviderBrowserLostRefreshRetiresOriginalOwnedClient(t *testing.T) {
	g, authority := browserGatewayFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if r.ParseForm() != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		successfulBrowserToken(w, r)
	}))
	location, proof := beginBrowserAuthorization(t, g)
	w := browserCallback(g, location, proof)
	require.Equal(t, http.StatusSeeOther, w.Code)
	r := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/local/run-a/prompt", nil)
	r.AddCookie(w.Result().Cookies()[0])
	session := g.session(r)
	require.NotNil(t, session)
	session.mu.Lock()
	config := g.oauth(session.clientID)
	session.tokens = config.TokenSource(session.ctx, &oauth2.Token{AccessToken: "expired", RefreshToken: "lost-refresh", Expiry: utils.Now().Add(-time.Minute)})
	session.mu.Unlock()
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Empty(t, authority.clients)
	require.Equal(t, 1, authority.retired)
	require.ErrorIs(t, session.ctx.Err(), context.Canceled)
}

func TestProviderBrowserDuplicateCookieAndHostIsolation(t *testing.T) {
	g, authority := browserGatewayFixture(t, http.HandlerFunc(successfulBrowserToken))
	for _, host := range []string{g.origin.Host, "admin.example.com"} {
		r := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/local/run-a/", nil)
		r.Host = host
		r.Header.Set("Accept", "text/html")
		r.AddCookie(&http.Cookie{Name: g.cookieName("session"), Value: "first"})
		r.AddCookie(&http.Cookie{Name: g.cookieName("session"), Value: "second"})
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		require.Equal(t, http.StatusForbidden, w.Code)
	}
	require.Zero(t, authority.created)
}
