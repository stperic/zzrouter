package server

import (
	"sort"
	"strings"
	"testing"
)

// The route catalog's `async` flag is the agent's cue to expect a job
// id instead of a result. It comes from a hand-maintained list, and
// that list has been wrong six times: /models/scans, /config/reload,
// /runs/batch and two install sub-steps were flagged async while
// answering synchronously, and DELETE /deployments inherited the flag
// from its POST sibling. Every one of those was found by driving the
// route, months apart, because nothing compared the list to anything.
//
// This compares it to openapi.yaml, which is drift-checked against the
// engine and whose 202s were read off the handlers by a human. Not
// proof that the server answers 202, but it does mean two independently
// maintained artifacts have to agree, and disagreeing is now a build
// failure rather than a surprise at runtime.

// async202Exempt lists routes that answer 202 with nothing to
// subscribe to, so the catalog flag would mislead rather than help.
// The flag means "202 plus a job id", not "202".
var async202Exempt = map[routeKey]string{
	rk("DELETE", "/zzrouter/v1/jobs/{id}"): "cancels a job; the 202 acknowledges the request, there is no new job to watch",
	rk("POST", "/zzrouter/v1/runs/batch"):  "launches inline and returns the outcomes; the work is done when the response arrives",
	rk("POST", "/zzrouter/v1/update/rollback"): "202 only when a privileged updater takes the request, and that runs " +
		"in another process; poll /update/status",
}

func TestRouteCatalog_AsyncFlagMatchesTheSpec(t *testing.T) {
	// Seeded so the config-store-gated routes register; without it the
	// install family is absent and this passes on a smaller surface.
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})
	spec := loadOpenAPI(t)

	var flaggedButSync, docAsyncButUnflagged, staleExempt []string
	checked, flagged := 0, 0

	for _, r := range s.engine.Routes() {
		if !isPublicAdminPath(r.Path) {
			continue
		}
		specPath := normalizeGinPath(r.Path)
		op, documented := spec.Paths[specPath][strings.ToLower(r.Method)]
		if !documented {
			// Both ratchets in openapi_drift_admin_test.go are empty, so
			// this cannot happen without that test failing first.
			continue
		}
		checked++

		var body struct {
			Responses map[string]struct{} `yaml:"responses"`
		}
		if err := op.Decode(&body); err != nil {
			t.Fatalf("decode %s %s: %v", r.Method, specPath, err)
		}
		_, spec202 := body.Responses["202"]
		catalogAsync := asyncForPath(r.Method, r.Path)
		if catalogAsync {
			flagged++
		}
		key := rk(r.Method, specPath)
		reason, exempt := async202Exempt[key]

		switch {
		case catalogAsync && !spec202:
			flaggedButSync = append(flaggedButSync, string(key)+
				" (catalog says async; the spec documents no 202)")
		case !catalogAsync && spec202 && !exempt:
			docAsyncButUnflagged = append(docAsyncButUnflagged, string(key)+
				" (spec documents 202; the catalog calls it synchronous)")
		case catalogAsync && exempt:
			staleExempt = append(staleExempt, string(key)+" — "+reason)
		}
	}

	if checked == 0 {
		t.Fatal("compared no routes; the spec lookup broke and this guard is blind")
	}
	if flagged == 0 {
		t.Fatal("no route is flagged async; asyncForPath stopped matching and this guard would pass vacuously")
	}
	t.Logf("compared %d documented management routes, %d flagged async", checked, flagged)

	sort.Strings(flaggedButSync)
	sort.Strings(docAsyncButUnflagged)
	sort.Strings(staleExempt)
	for _, m := range flaggedButSync {
		t.Errorf("%s\nAn agent flagged here waits for a job id that never comes. "+
			"Drive the route: if it answers synchronously, remove it from asyncRoutes.", m)
	}
	for _, m := range docAsyncButUnflagged {
		t.Errorf("%s\nAdd it to asyncRoutes, or if its 202 carries no job id, "+
			"add it to async202Exempt with the reason.", m)
	}
	for _, m := range staleExempt {
		t.Errorf("exempt route is also flagged async, so one of the two is wrong: %s", m)
	}
}
