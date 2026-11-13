package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/oauth2"
)

const (
	providerBrowserResource          = "/provider-ui"
	providerBrowserScope             = "zzrouter:provider-ui"
	providerBrowserOwner             = "provider-ui"
	providerBrowserSessionTTL        = 12 * time.Hour
	providerBrowserPendingTTL        = 5 * time.Minute
	providerBrowserMaxSessions       = 32
	providerBrowserMaxPendingPerPeer = 4
)

// The consumer owns opaque browser sessions; the authority owns clients and durable grants.
type providerBrowserAuthority interface {
	CreateClient(context.Context) (string, error)
	RetireClient(context.Context, string) error
	ResetClients(context.Context) error
	Authenticate(context.Context, string) (*AccessContext, error)
}

type providerBrowserAuthorization struct {
	mu                                      sync.Mutex // serializes client creation, refresh and retirement
	state, clientID, verifier, target, peer string
	binding                                 [32]byte
	cookie                                  [32]byte
	expires                                 time.Time
	consumed, retiring                      bool // protected by gateway.mu
	ctx                                     context.Context
	cancel                                  context.CancelFunc
	tokens                                  oauth2.TokenSource
}

type providerBrowserGateway struct {
	server     *Server
	origin     *url.URL
	issuer     string
	authority  providerBrowserAuthority
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closing    bool
	states     map[string]*providerBrowserAuthorization
	sessions   map[[32]byte]*providerBrowserAuthorization
	owners     sync.WaitGroup
	handlers   sync.WaitGroup
	engine     *gin.Engine
	httpServer *http.Server
}

func newProviderBrowserGateway(ctx context.Context, server *Server, origin, issuer string, authority providerBrowserAuthority, client *http.Client) (*providerBrowserGateway, error) {
	if err := authority.ResetClients(ctx); err != nil {
		return nil, err
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	oauthHTTP := *client
	oauthHTTP.Timeout = 10 * time.Second
	oauthHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithCancel(context.WithValue(ctx, oauth2.HTTPClient, &oauthHTTP)) //nolint:gosec // The gateway owns cancel and invokes it in close.
	g := &providerBrowserGateway{server: server, origin: u, issuer: issuer, authority: authority, ctx: ctx, cancel: cancel, states: map[string]*providerBrowserAuthorization{}, sessions: map[[32]byte]*providerBrowserAuthorization{}}
	g.engine = gin.New()
	g.engine.RedirectTrailingSlash = false
	g.engine.UseRawPath = true
	g.engine.Any("/*path", g.serve)
	g.owners.Go(g.reap)
	return g, nil
}

func (g *providerBrowserGateway) oauth(clientID string) oauth2.Config {
	return oauth2.Config{ClientID: clientID, RedirectURL: g.origin.String() + "/oauth/callback", Scopes: []string{providerBrowserScope, "offline_access"}, Endpoint: oauth2.Endpoint{AuthURL: g.issuer + "/oauth/authorize", TokenURL: g.issuer + "/oauth/token", AuthStyle: oauth2.AuthStyleInParams}}
}

func (g *providerBrowserGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.closing || g.ctx.Err() != nil {
		g.mu.Unlock()
		http.Error(w, "Provider browser unavailable.", http.StatusServiceUnavailable)
		return
	}
	g.handlers.Add(1)
	g.mu.Unlock()
	defer g.handlers.Done()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	if !providerBrowserRequestAllowed(r, g.origin) {
		http.Error(w, "Provider browser origin refused.", http.StatusForbidden)
		return
	}
	for _, name := range []string{g.cookieName("session"), g.cookieName("preauth")} {
		if len(r.CookiesNamed(name)) > 1 {
			http.Error(w, "Ambiguous browser cookies. Clear cookies and sign in again.", http.StatusForbidden)
			return
		}
	}
	ctx, cancel := providerRequestContext(g.ctx, r.Context())
	defer cancel()
	g.engine.ServeHTTP(w, r.Clone(ctx))
}

func (g *providerBrowserGateway) serve(c *gin.Context) {
	r := c.Request
	if r.URL.Path == "/oauth/callback" && r.Method == http.MethodGet {
		g.callback(c)
		return
	}
	if r.URL.Path == "/logout" && r.Method == http.MethodPost {
		if session := g.session(r); session != nil {
			if err := g.retire(r.Context(), session); err != nil {
				http.Error(c.Writer, "Could not retire browser authorization.", http.StatusServiceUnavailable)
				return
			}
		}
		g.setCookie(c.Writer, "session", "", -1)
		c.Status(http.StatusNoContent)
		return
	}
	destination, ok := parseProviderBrowserTarget(r.URL)
	if !ok || !g.server.providerBrowserNodeKnown(destination.node) {
		http.NotFound(c.Writer, r)
		return
	}
	session := g.session(r)
	if session == nil {
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			g.begin(c, destination.prefix+destination.suffix.String())
		} else {
			http.Error(c.Writer, "Sign in to use this provider.", http.StatusUnauthorized)
		}
		return
	}
	ac, err := g.authenticate(session)
	if err != nil {
		_ = g.retire(r.Context(), session)
		g.setCookie(c.Writer, "session", "", -1)
		http.Error(c.Writer, "Browser authorization expired or was revoked. Sign in again.", http.StatusUnauthorized)
		return
	}
	setAccessContext(c, ac)
	if !g.server.admitOpaqueForward(c, "provider browser", false) {
		return
	}
	_, release, allowed := g.server.enforceAndAttribute(c, "provider browser", nil, nil)
	defer release()
	if !allowed {
		return
	}
	ctx, cancel := providerRequestContext(session.ctx, r.Context())
	defer cancel()
	g.server.forwardProviderBrowser(c.Writer, r.WithContext(ctx), destination)
}

func (g *providerBrowserGateway) cookieName(kind string) string {
	name := "zzrouter_provider_" + kind
	if g.origin.Scheme == "https" {
		return "__Host-" + name
	}
	return name
}

func (g *providerBrowserGateway) setCookie(w http.ResponseWriter, kind, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: g.cookieName(kind), Value: value, Path: "/", HttpOnly: true, Secure: g.origin.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (g *providerBrowserGateway) session(r *http.Request) *providerBrowserAuthorization {
	cookie, err := r.Cookie(g.cookieName("session"))
	if err != nil || cookie.Value == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	session := g.sessions[sha256.Sum256([]byte(cookie.Value))]
	if session == nil || session.retiring || !utils.Now().Before(session.expires) {
		return nil
	}
	return session
}

func (g *providerBrowserGateway) begin(c *gin.Context, target string) {
	proof := ""
	if cookie, err := c.Request.Cookie(g.cookieName("preauth")); err == nil && len(cookie.Value) == 52 {
		proof = cookie.Value
	}
	if proof == "" {
		proof = rand.Text() + rand.Text()
	}
	state := rand.Text() + rand.Text()
	ctx, cancel := context.WithCancel(g.ctx)
	a := &providerBrowserAuthorization{state: state, verifier: oauth2.GenerateVerifier(), target: target, peer: providerBrowserPeer(c.Request), binding: sha256.Sum256([]byte(proof)), expires: utils.Now().Add(providerBrowserPendingTTL), ctx: ctx, cancel: cancel}
	g.mu.Lock()
	pending := 0
	for _, existing := range g.states {
		if existing.peer == a.peer && existing.cookie == ([32]byte{}) {
			pending++
		}
	}
	if g.closing || len(g.states) >= providerBrowserMaxSessions || pending >= providerBrowserMaxPendingPerPeer {
		g.mu.Unlock()
		cancel()
		http.Error(c.Writer, "Browser authorization capacity reached. Try again later.", http.StatusTooManyRequests)
		return
	}
	g.states[state] = a
	g.mu.Unlock()
	a.mu.Lock()
	var createErr error
	a.clientID, createErr = g.authority.CreateClient(c.Request.Context())
	clientID := a.clientID
	a.mu.Unlock()
	if createErr != nil || clientID == "" || a.ctx.Err() != nil {
		_ = g.retire(c.Request.Context(), a)
		http.Error(c.Writer, "Could not start browser authorization.", http.StatusServiceUnavailable)
		return
	}
	g.setCookie(c.Writer, "preauth", proof, int(providerBrowserPendingTTL.Seconds()))
	config := g.oauth(clientID)
	location := config.AuthCodeURL(state, oauth2.S256ChallengeOption(a.verifier), oauth2.SetAuthURLParam("resource", g.issuer+providerBrowserResource))
	http.Redirect(c.Writer, c.Request, location, http.StatusSeeOther)
}

func providerBrowserPeer(r *http.Request) string {
	if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return peer.Addr().Unmap().String()
	}
	if peer, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return peer.Unmap().String()
	}
	return "unknown"
}

func (g *providerBrowserGateway) callback(c *gin.Context) {
	r := c.Request
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 8192 {
		http.Error(c.Writer, "Invalid authorization callback.", http.StatusBadRequest)
		return
	}
	for _, key := range []string{"state", "code", "iss", "error"} {
		if len(query[key]) > 1 {
			http.Error(c.Writer, "Repeated authorization callback parameter.", http.StatusBadRequest)
			return
		}
	}
	proof, err := r.Cookie(g.cookieName("preauth"))
	if err != nil {
		http.Error(c.Writer, "Authorization browser proof missing.", http.StatusForbidden)
		return
	}
	binding := sha256.Sum256([]byte(proof.Value))
	g.mu.Lock()
	a := g.states[query.Get("state")]
	valid := a != nil && !a.consumed && !a.retiring && utils.Now().Before(a.expires) && subtle.ConstantTimeCompare(a.binding[:], binding[:]) == 1
	if valid {
		a.consumed = true
	}
	g.mu.Unlock()
	if !valid {
		http.Error(c.Writer, "Authorization callback expired or already consumed.", http.StatusForbidden)
		return
	}
	if query.Get("iss") != g.issuer || query.Get("error") != "" || query.Get("code") == "" {
		_ = g.retire(r.Context(), a)
		http.Error(c.Writer, "Authorization was not completed.", http.StatusForbidden)
		return
	}
	ctx, cancel := providerRequestContext(a.ctx, r.Context())
	defer cancel()
	a.mu.Lock()
	config := g.oauth(a.clientID)
	token, err := config.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(a.verifier), oauth2.SetAuthURLParam("resource", g.issuer+providerBrowserResource))
	if err == nil {
		a.tokens = config.TokenSource(a.ctx, token)
	}
	a.mu.Unlock()
	if err == nil {
		_, err = g.authenticate(a)
	}
	if err != nil {
		_ = g.retire(r.Context(), a)
		http.Error(c.Writer, "Could not complete browser authorization. Sign in again.", http.StatusUnauthorized)
		return
	}
	if old := g.session(r); old != nil {
		_ = g.retire(r.Context(), old)
	}
	secret := rand.Text() + rand.Text()
	g.mu.Lock()
	if g.closing || a.retiring || a.ctx.Err() != nil {
		g.mu.Unlock()
		_ = g.retire(r.Context(), a)
		http.Error(c.Writer, "Browser authorization closed.", http.StatusServiceUnavailable)
		return
	}
	a.cookie, a.expires, a.verifier = sha256.Sum256([]byte(secret)), utils.Now().Add(providerBrowserSessionTTL), ""
	g.sessions[a.cookie] = a
	g.owners.Go(func() { g.watch(a) })
	g.mu.Unlock()
	g.setCookie(c.Writer, "session", secret, int(providerBrowserSessionTTL.Seconds()))
	http.Redirect(c.Writer, r, a.target, http.StatusSeeOther)
}

func (g *providerBrowserGateway) authenticate(a *providerBrowserAuthorization) (*AccessContext, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil || a.tokens == nil {
		return nil, errors.New("browser session closed")
	}
	token, err := a.tokens.Token()
	if err != nil {
		return nil, err
	}
	ac, err := g.authority.Authenticate(a.ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}
	if ac == nil || ac.Key == nil {
		return nil, errors.New("browser identity unavailable")
	}
	if why := opaqueForwardLimit(ac); why != "" {
		return nil, errors.New(why)
	}
	return ac, nil
}

func (g *providerBrowserGateway) retire(ctx context.Context, a *providerBrowserAuthorization) error {
	g.mu.Lock()
	a.retiring = true
	a.cancel()
	g.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clientID != "" {
		if err := g.authority.RetireClient(ctx, a.clientID); err != nil {
			return err
		}
	}
	g.mu.Lock()
	delete(g.states, a.state)
	delete(g.sessions, a.cookie)
	g.mu.Unlock()
	return nil
}

func (g *providerBrowserGateway) watch(a *providerBrowserAuthorization) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
			if _, err := g.authenticate(a); err != nil {
				_ = g.retire(g.ctx, a)
				return
			}
		}
	}
}

func (g *providerBrowserGateway) reap() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-tick.C:
			g.expire(g.ctx)
		}
	}
}

func (g *providerBrowserGateway) expire(ctx context.Context) {
	g.mu.Lock()
	var expired []*providerBrowserAuthorization
	for _, a := range g.states {
		if a.retiring || !utils.Now().Before(a.expires) {
			expired = append(expired, a)
		}
	}
	g.mu.Unlock()
	for _, a := range expired {
		_ = g.retire(ctx, a)
	}
}

func (g *providerBrowserGateway) close(ctx context.Context) error {
	g.mu.Lock()
	g.closing = true
	g.cancel()
	g.mu.Unlock()
	if g.httpServer != nil {
		_ = g.httpServer.Close()
	}
	done := make(chan struct{})
	go func() { g.handlers.Wait(); g.owners.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	return g.authority.ResetClients(ctx)
}
