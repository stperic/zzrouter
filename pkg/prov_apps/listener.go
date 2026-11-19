package prov_apps

import (
	"context"
	"net"
	"net/url"
	"strconv"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// RunUpstreamURL reads the canonical endpoint of one exact live run without
// acquiring a lease, starting a process or probing the endpoint.
func (m *ProviderAppManager) RunUpstreamURL(ctx context.Context, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	run, found := m.instances.Get(id)
	if !found || run.GetStatus() != instance.StatusRunning || run.Context().Err() != nil {
		return "", ErrProviderUnavailable
	}
	endpoint, _, err := m.Upstream(run)
	if err != nil {
		return "", err
	}
	current, present := m.instances.Get(id)
	if !present || current != run || run.GetStatus() != instance.StatusRunning || run.Context().Err() != nil {
		return "", ErrProviderUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return endpoint, nil
}

func launchListener(svc config.ServiceConfig, parameters, environment map[string]string, port int) *instance.ListenerInfo {
	view := &instance.ListenerInfo{Scope: "unknown", Authentication: "unknown", Source: "launch_configuration"}
	if svc.Runtime == nil || svc.Runtime.Listener == nil || port < 1 {
		return view
	}
	declared := svc.Runtime.Listener
	host := declared.DefaultHost
	if value := environment[declared.HostEnvironment]; value != "" {
		host = value
	}
	if value := parameters[declared.HostParameter]; value != "" {
		host = value
	}
	ip := net.ParseIP(host)
	if ip == nil && host != "localhost" {
		return view
	}
	view.Host = host
	switch {
	case host == "localhost" || ip.IsLoopback():
		view.Scope = "loopback"
	case ip.IsUnspecified():
		view.Scope = "all_interfaces"
	default:
		view.Scope = "interface"
	}
	// An unspecified bind is not a destination address. The node owns loopback too.
	destination := host
	if ip != nil && ip.IsUnspecified() {
		destination = "127.0.0.1"
	}
	view.URL = (&url.URL{Scheme: "http", Host: net.JoinHostPort(destination, strconv.Itoa(port))}).String()
	return view
}
