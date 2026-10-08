package servercli

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// updateListenDeadline is how long a freshly updated node has to open
// its port before the update is given up on.
//
// Generous because the slow part is not the binary: a node reloads its
// model registry and re-establishes cluster state before it listens,
// and a coordinator with a large catalog takes minutes on a cold cache.
// Rolling back a working release because it was slow is worse than
// waiting.
const updateListenDeadline = 5 * time.Minute

// updateHealthDeadline is how long the node has to answer /health once
// it is listening. Short: a process that accepts connections and cannot
// answer its own health endpoint is broken, not busy.
const updateHealthDeadline = 90 * time.Second

// updateHealthPollInterval is how often the probes above retry.
const updateHealthPollInterval = 2 * time.Second

// updateHealthProbeTimeout bounds one probe.
const updateHealthProbeTimeout = 3 * time.Second

// nodeProbe answers "is the node on this machine serving" from outside
// the node's own process.
//
// Shared by the two halves of a verified update, which ask the same
// question from different places: the node itself, confirming the
// version it just booted on, and the privileged updater, confirming the
// version it just restarted the node onto. One implementation because a
// disagreement between them would show up as a healthy node being
// rolled back.
type nodeProbe struct {
	cfg *pkgConfig.NodeConfig
}

// addresses lists where this node's admin port might be reachable from
// on this machine, most specific first.
//
// The listener binds node.bind, which is a wildcard by default but may
// be pinned to one interface; probing only loopback would then never
// connect and would roll back a healthy release. Workers have their
// listener forced to loopback regardless of node.bind, so both are
// tried rather than picking one.
func (p nodeProbe) addresses() []string {
	hosts := make([]string, 0, 3)
	switch bind := p.cfg.Node.Bind; bind {
	case "", "0.0.0.0", "::", "[::]":
	default:
		hosts = append(hosts, bind)
	}
	return append(hosts, "127.0.0.1", "localhost")
}

// portOpen reports whether anything is accepting on the admin port yet.
func (p nodeProbe) portOpen() bool {
	for _, host := range p.addresses() {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(p.cfg.Node.Port)), updateHealthProbeTimeout)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

// healthClient dials the node's own admin port. Certificate validation
// is skipped because the peer is on this machine: the connection never
// leaves it, and a node with a self-signed cert would otherwise fail
// its own probe and roll back a working release.
func (p nodeProbe) healthClient() *http.Client {
	client := &http.Client{Timeout: updateHealthProbeTimeout}
	if p.cfg.Node.IsTLSEnabled() {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-probe over this node's own listener
		}
	}
	return client
}

// healthy reports whether the node answers its health endpoint.
func (p nodeProbe) healthy(client *http.Client) bool {
	scheme := "http"
	if p.cfg.Node.IsTLSEnabled() {
		scheme = "https"
	}
	for _, host := range p.addresses() {
		if CheckHealth(client, fmt.Sprintf("%s://%s/health", scheme,
			net.JoinHostPort(host, fmt.Sprint(p.cfg.Node.Port)))) {
			return true
		}
	}
	return false
}

// waitFor polls until ready reports true, the deadline passes, or the
// caller's abort channel closes.
func (p nodeProbe) waitFor(abort <-chan struct{}, deadline time.Duration, ready func() bool) bool {
	timeout := time.After(deadline)
	ticker := time.NewTicker(updateHealthPollInterval)
	defer ticker.Stop()

	for {
		if ready() {
			return true
		}
		select {
		case <-abort:
			return false
		case <-timeout:
			return false
		case <-ticker.C:
		}
	}
}

// waitUntilServing waits for the node to open its port and then answer
// /health, and says which step it did not get past.
//
// The health endpoint rather than "the process stayed up", because the
// failure worth catching is a binary that runs but cannot serve.
func (p nodeProbe) waitUntilServing(abort <-chan struct{}) error {
	if !p.waitFor(abort, updateListenDeadline, p.portOpen) {
		return fmt.Errorf("nothing is listening on %s:%d after %s",
			p.addresses()[0], p.cfg.Node.Port, updateListenDeadline)
	}
	client := p.healthClient()
	if !p.waitFor(abort, updateHealthDeadline, func() bool { return p.healthy(client) }) {
		return fmt.Errorf("it listened but did not answer /health within %s", updateHealthDeadline)
	}
	return nil
}
