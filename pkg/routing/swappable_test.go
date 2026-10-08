package routing

import (
	"context"
	"sync"
	"testing"
)

func TestSwappable_SwapRedirectsCalls(t *testing.T) {
	a := NewLocalOnlyRouterWithClient("a", "9090", nil, nil)
	b := NewLocalOnlyRouterWithClient("b", "9090", nil, nil)

	s := NewSwappable(a)
	if s.Current() != a {
		t.Fatalf("expected initial router to be a")
	}

	prev := s.Swap(b)
	if prev != a {
		t.Fatalf("Swap should return previous router")
	}
	if s.Current() != b {
		t.Fatalf("expected current router to be b after swap")
	}
}

func TestSwappable_ConcurrentSwapAndRead(t *testing.T) {
	a := NewLocalOnlyRouterWithClient("a", "9090", nil, nil)
	b := NewLocalOnlyRouterWithClient("b", "9090", nil, nil)
	s := NewSwappable(a)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = s.Current()
			}
		}()
	}
	for i := 0; i < 1000; i++ {
		if i%2 == 0 {
			s.Swap(b)
		} else {
			s.Swap(a)
		}
	}
	wg.Wait()
	// -race catches any torn read.
}

type countingRouter struct {
	unicast, broadcast, route, multi int
}

func (c *countingRouter) Unicast(context.Context, string, string, string, []byte) (*Response, error) {
	c.unicast++
	return nil, nil
}
func (c *countingRouter) Broadcast(context.Context, string, string, []byte) (*Response, error) {
	c.broadcast++
	return nil, nil
}
func (c *countingRouter) Route(context.Context, *Request) (*Response, error) {
	c.route++
	return nil, nil
}
func (c *countingRouter) MultiRoute(context.Context, string, string, map[string][]byte) (*Response, error) {
	c.multi++
	return nil, nil
}

func TestSwappable_DelegatesAllMethods(t *testing.T) {
	c := &countingRouter{}
	s := NewSwappable(c)
	ctx := context.Background()
	_, _ = s.Unicast(ctx, "", "", "", nil)
	_, _ = s.Broadcast(ctx, "", "", nil)
	_, _ = s.Route(ctx, &Request{})
	_, _ = s.MultiRoute(ctx, "", "", nil)
	if c.unicast != 1 || c.broadcast != 1 || c.route != 1 || c.multi != 1 {
		t.Fatalf("expected each method to delegate once, got %+v", c)
	}
}
