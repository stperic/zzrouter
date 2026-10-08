package client

import "net/url"

// RunsReport is what a provider write says about the models already
// running: which would now launch differently, which hold their own value
// for a changed key, and which cannot say.
type RunsReport struct {
	Stale        []RunRef `json:"stale"`
	Overridden   []RunRef `json:"overridden,omitempty"`
	Unknown      []RunRef `json:"unknown,omitempty"`
	RestartJobID string   `json:"restart_job_id,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// RunRef names one run in a RunsReport.
type RunRef struct {
	ID         string   `json:"id"`
	Node       string   `json:"node"`
	Model      string   `json:"model"`
	Changed    []string `json:"changed,omitempty"`
	Overridden []string `json:"overridden,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// writeQuery is the query a provider write sends: with restart, the runs
// the write leaves stale are restarted.
func writeQuery(restart bool) url.Values {
	q := url.Values{}
	if restart {
		q.Set("restart", "affected")
	}
	return q
}

// withQuery appends q to path when it holds anything.
func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}
