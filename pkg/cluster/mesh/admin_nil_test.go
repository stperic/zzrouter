package mesh

import (
	"context"
	"errors"
	"testing"
)

// A Cluster with no connector must report, not panic.
//
// Both validation routes mount unconditionally on the public admin
// engine, and a worker never builds a coordinator, so a worker serves
// them with a nil *Cluster. These methods used to construct a throwaway
// Connector and so never touched the receiver, which made a nil one
// harmless by accident. Reusing the stored connector removed that
// accident: without these guards the same request is a nil dereference.
func TestValidateNode_NoConnector(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *Cluster
	}{
		{"nil cluster", nil},
		{"nil connector", &Cluster{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := tc.c.ValidateNode(context.Background(), "http://10.0.0.1:9090")
			if !errors.Is(err, ErrNoConnector) {
				t.Fatalf("err = %v, want ErrNoConnector", err)
			}
			if conn != nil {
				t.Fatalf("conn = %v, want nil", conn)
			}
		})
	}
}

func TestValidateAllConnections_NoConnector(t *testing.T) {
	urls := []string{"http://10.0.0.1:9090", "http://10.0.0.2:9090"}

	for _, tc := range []struct {
		name string
		c    *Cluster
	}{
		{"nil cluster", nil},
		{"nil connector", &Cluster{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.ValidateAllConnections(context.Background(), urls)
			if len(got) != len(urls) {
				t.Fatalf("len = %d, want %d", len(got), len(urls))
			}
			// Every row must carry the URL and an error: a caller that
			// partitions on Err != nil would otherwise count a
			// zero-value row as a healthy host with an empty URL.
			for i, h := range got {
				if h.URL != urls[i] {
					t.Errorf("row %d URL = %q, want %q", i, h.URL, urls[i])
				}
				if !errors.Is(h.Err, ErrNoConnector) {
					t.Errorf("row %d Err = %v, want ErrNoConnector", i, h.Err)
				}
			}
		})
	}
}
