package signal

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/stperic/common/retry"
)

// CoordinatorConfig binds the exchange to current membership and existing services.
type CoordinatorConfig struct {
	Client func() *http.Client
	Member func(url, node string) bool
	Handle func(context.Context, *x509.Certificate, Operation) Result
}

// Coordinator owns polls and concurrent operations for the current coordinator role.
type Coordinator struct {
	cfg        CoordinatorConfig
	mu         sync.Mutex
	generation *generation
}
type generation struct {
	ctx    context.Context
	cancel context.CancelFunc
	peers  map[string]*peer
	wg     sync.WaitGroup
}
type peer struct {
	name   string
	cancel context.CancelFunc
}

// NewCoordinator constructs a dormant poll owner.
func NewCoordinator(cfg CoordinatorConfig) *Coordinator { return &Coordinator{cfg: cfg} }

// Start opens an independent coordinator lifecycle generation.
func (c *Coordinator) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: Stop owns the generation cancellation.
	c.generation = &generation{ctx: ctx, cancel: cancel, peers: make(map[string]*peer)}
}

// Stop closes admission before joining this generation's polls and operations.
func (c *Coordinator) Stop(ctx context.Context) {
	c.mu.Lock()
	g := c.generation
	c.generation = nil
	if g != nil {
		g.cancel()
	}
	c.mu.Unlock()
	if g == nil {
		return
	}
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Observe ensures one poll owner for a registered peer.
func (c *Coordinator) Observe(url, node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.generation
	if g == nil || g.ctx.Err() != nil || !c.cfg.Member(url, node) {
		return
	}
	if p := g.peers[url]; p != nil {
		if p.name == node {
			return
		}
		p.cancel()
	}
	ctx, cancel := context.WithCancel(g.ctx)
	p := &peer{name: node, cancel: cancel}
	g.peers[url] = p
	g.wg.Add(1)
	go func() {
		defer cancel()
		defer g.wg.Done()
		defer func() {
			c.mu.Lock()
			if g.peers[url] == p {
				delete(g.peers, url)
			}
			c.mu.Unlock()
		}()
		c.run(ctx, url, node)
	}()
}

type operationState struct{ result *Result }
type exchange struct {
	mu          sync.Mutex
	incarnation string
	operations  map[string]*operationState
	pollCancel  context.CancelFunc
	cancel      context.CancelFunc
	ctx         context.Context
	wg          sync.WaitGroup
}

func newExchange(ctx context.Context) *exchange {
	ctx, cancel := context.WithCancel(ctx)
	return &exchange{operations: make(map[string]*operationState), ctx: ctx, cancel: cancel}
}
func (x *exchange) stop() { x.cancel(); x.wg.Wait() }

func (c *Coordinator) run(ctx context.Context, url, node string) {
	backoff := retry.Backoff{Initial: time.Second, Max: 10 * time.Second, Multiplier: 2, Jitter: 0.2}
	x := newExchange(ctx)
	defer func() { x.stop() }()
	for failures := 0; ctx.Err() == nil && c.cfg.Member(url, node); failures++ {
		err := c.exchange(ctx, url, node, x)
		if errors.Is(err, errIncarnationChanged) {
			x.stop()
			x = newExchange(ctx)
		}
		timer := time.NewTimer(backoff.Delay(failures))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}

}

var errIncarnationChanged = errors.New("node exchange incarnation changed")

//nolint:contextcheck // Operations belong to the exchange, independently of each parked poll.
func (c *Coordinator) exchange(ctx context.Context, url, node string, x *exchange) error {
	for ctx.Err() == nil && c.cfg.Member(url, node) {
		x.mu.Lock()
		ack := Acknowledgment{Incarnation: x.incarnation}
		poll := Poll{Incarnation: x.incarnation}
		for id, op := range x.operations {
			if op.result != nil {
				ack.Results = append(ack.Results, *op.result)
			} else {
				poll.Seen = append(poll.Seen, id)
			}
		}
		x.mu.Unlock()
		if len(ack.Results) > 0 {
			ackCtx, ackCancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := c.call(ackCtx, url+"/zzrouter/v1/internal/signals/ack", ack, nil)
			ackCancel()
			if err != nil {
				return err
			}
			x.mu.Lock()
			for _, r := range ack.Results {
				delete(x.operations, r.ID)
			}
			x.mu.Unlock()
		}
		pollCtx, cancel := context.WithTimeout(ctx, Hold+10*time.Second)
		x.mu.Lock()
		x.pollCancel = cancel
		// An operation may have completed after the snapshot, before this poll was parked.
		completed := false
		for _, op := range x.operations {
			if op.result != nil {
				completed = true
				break
			}
		}
		x.mu.Unlock()
		if completed {
			cancel()
			continue
		}
		var batch Batch
		cert, err := c.call(pollCtx, url+"/zzrouter/v1/internal/signals/poll", poll, &batch)
		cancel()
		x.mu.Lock()
		x.pollCancel = nil
		x.mu.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Local completion interrupts the parked poll so its result can be acknowledged.
			if errors.Is(err, context.Canceled) {
				continue
			}
			return err
		}
		if batch.Incarnation == "" {
			return errors.New("node exchange has no incarnation")
		}
		if cert == nil || cert.Subject.CommonName != node {
			return errors.New("node exchange identity mismatch")
		}
		x.mu.Lock()
		if x.incarnation != "" && x.incarnation != batch.Incarnation {
			x.mu.Unlock()
			return errIncarnationChanged
		}
		x.incarnation = batch.Incarnation
		for _, op := range batch.Operations {
			if op.ID == "" {
				x.mu.Unlock()
				return errors.New("node exchange operation has no identity")
			}
			if x.operations[op.ID] != nil {
				continue
			}
			if len(x.operations) >= maxPending {
				x.mu.Unlock()
				return errors.New("node exchange response exceeds capacity")
			}
			state := &operationState{}
			x.operations[op.ID] = state
			x.wg.Add(1)
			go func() {
				defer x.wg.Done()
				opCtx, opCancel := context.WithTimeout(x.ctx, 60*time.Second)
				defer opCancel()
				result := c.cfg.Handle(opCtx, cert, op)
				result.ID = op.ID
				x.mu.Lock()
				state.result = &result
				if x.pollCancel != nil {
					x.pollCancel()
				}
				x.mu.Unlock()
			}()
		}
		x.mu.Unlock()
	}
	return ctx.Err()
}
func (c *Coordinator) call(ctx context.Context, url string, input, output any) (*x509.Certificate, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Each poll authenticates the current node certificate, including after renewal.
	req.Close = true
	client := c.cfg.Client()
	if client == nil {
		return nil, errors.New("coordinator mTLS client is unavailable")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil, errIncarnationChanged
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node exchange returned status %d", resp.StatusCode)
	}
	if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 || len(resp.TLS.PeerCertificates) == 0 {
		return nil, errors.New("node exchange requires verified mTLS")
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(output); err != nil {
			return nil, err
		}
	}
	return resp.TLS.PeerCertificates[0], nil
}
