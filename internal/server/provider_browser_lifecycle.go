package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

func (s *Server) startProviderBrowser(ctx context.Context) error {
	settings := s.config.Coordinator.ProviderBrowser
	if settings.Origin == "" {
		return nil
	}
	if err := settings.Validate(s.config.Auth.OAuth, s.config.Node); err != nil {
		return err
	}
	s.providerBrowser.Lock()
	defer s.providerBrowser.Unlock()
	if s.providerBrowser.gateway != nil {
		return nil
	}
	if s.providerBrowser.authority == nil {
		return errors.New("provider browser authorization is unavailable")
	}
	issuer, err := s.config.Auth.OAuth.EffectiveIssuer(s.config.Node)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", settings.ListenAddress())
	if err != nil {
		return fmt.Errorf("listen for provider browser: %w", err)
	}
	gateway, err := newProviderBrowserGateway(ctx, s, settings.Origin, issuer, s.providerBrowser.authority, s.httpClient)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("initialize provider browser: %w", err)
	}
	gateway.httpServer = &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return gateway.ctx }}
	s.providerBrowser.gateway = gateway
	gateway.owners.Go(func() {
		err := gateway.httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			gateway.cancel()
			select {
			case s.servingErrors <- fmt.Errorf("serve provider browser: %w", err):
			case <-ctx.Done():
			}
		}
	})
	return nil
}

func (s *Server) stopProviderBrowser(ctx context.Context) error {
	s.providerBrowser.Lock()
	defer s.providerBrowser.Unlock()
	gateway := s.providerBrowser.gateway
	if gateway == nil {
		return nil
	}
	// Remove observations before closing streams or retiring authorization.
	s.providerBrowser.gateway = nil
	return gateway.close(ctx)
}

func (s *Server) providerBrowserURL(node, id string) string {
	if _, ok := providerBrowserSegment(url.PathEscape(id)); !ok {
		return ""
	}
	if _, ok := providerBrowserSegment(url.PathEscape(node)); !ok {
		return ""
	}
	if s.readinessUnavailable() != "" {
		return ""
	}
	s.providerBrowser.RLock()
	defer s.providerBrowser.RUnlock()
	gateway := s.providerBrowser.gateway
	if gateway == nil || gateway.ctx.Err() != nil || !s.providerBrowserNodeKnown(node) {
		return ""
	}
	return gateway.origin.String() + "/runs/" + url.PathEscape(node) + "/" + url.PathEscape(id) + "/"
}
