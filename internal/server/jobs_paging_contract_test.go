package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoteJobsPagingContract(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		// Synthetic worker fixture with equal timestamps exercises the id tie-breaker.
		_, err := io.WriteString(w, `{"node":"w1","jobs":[{"job_id":"c"},{"job_id":"a"},{"job_id":"b"}]}`)
		require.NoError(t, err)
	}))
	defer up.Close()
	c := newProxyController(up.URL, false)
	for _, tc := range []struct {
		query, id string
		status    int
		more      bool
	}{
		{"?node=w1&limit=1", "a", http.StatusOK, true},
		{"?node=w1&limit=1&offset=1", "b", http.StatusOK, true},
		{"?node=w1&limit=1&offset=2", "c", http.StatusOK, false},
		{"?node=w1&offset=-1", "", http.StatusBadRequest, false},
		{"?node=w1&limit=0", "", http.StatusBadRequest, false},
		{"?node=w1&offset=x", "", http.StatusBadRequest, false},
	} {
		w := serveHandler(c.ListJobs, "/jobs", "/jobs"+tc.query)
		require.Equal(t, tc.status, w.Code, w.Body.String())
		if tc.status == http.StatusOK {
			var body struct {
				Data []struct {
					JobID string `json:"job_id"`
				}
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Len(t, body.Data, 1)
			require.Equal(t, tc.id, body.Data[0].JobID)
			require.Equal(t, 3, body.Total)
			require.Equal(t, tc.more, body.HasMore)
		}
	}
	require.EqualValues(t, 3, calls.Load(), "invalid paging must not reach the worker")
}

func TestRemoteJobsRecentFirstContract(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Synthetic timestamps separate recency ordering from the id tie-breaker.
		_, err := io.WriteString(w, `{"node":"w1","jobs":[{"job_id":"a","at":"2026-10-11T09:00:00Z"},{"job_id":"c","at":"2026-10-11T10:00:00Z"},{"job_id":"b","at":"2026-10-11T10:00:00Z"}]}`)
		require.NoError(t, err)
	}))
	defer up.Close()
	c := newProxyController(up.URL, false)
	w := serveHandler(c.ListJobs, "/jobs", "/jobs?node=w1&limit=1")
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Data []struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.Equal(t, "b", body.Data[0].JobID)
}
