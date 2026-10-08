package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// sentinelStatus is the decision record: every error prov_apps exports
// at the HTTP boundary, and the status it must produce.
//
// 500 is the default extractStatusCode falls back to, which is why a
// sentinel missing from this table is a bug rather than a shrug: it
// means a failure the caller caused is being reported as a failure the
// server caused, and an agent reading 500 will retry something that
// cannot succeed. Adding a sentinel to prov_apps without adding a row
// here fails the test below.
var sentinelStatus = map[string]struct {
	err  error
	want int
}{
	"ErrProviderNotFound":         {prov_apps.ErrProviderNotFound, http.StatusNotFound},
	"ErrInstanceNotFound":         {prov_apps.ErrInstanceNotFound, http.StatusNotFound},
	"ErrInstallInProgress":        {prov_apps.ErrInstallInProgress, http.StatusConflict},
	"ErrInstancesRunning":         {prov_apps.ErrInstancesRunning, http.StatusConflict},
	"ErrProviderNotManaged":       {prov_apps.ErrProviderNotManaged, http.StatusConflict},
	"ErrProviderAlreadyInstalled": {prov_apps.ErrProviderAlreadyInstalled, http.StatusConflict},
	"ErrInstanceAlreadyExists":    {prov_apps.ErrInstanceAlreadyExists, http.StatusConflict},
	"ErrModelAlreadyLoaded":       {prov_apps.ErrModelAlreadyLoaded, http.StatusConflict},
	"ErrPortConflict":             {prov_apps.ErrPortConflict, http.StatusConflict},
	"ErrModelIncomplete":          {prov_apps.ErrModelIncomplete, http.StatusConflict},
	"ErrUnsupportedPlatform":      {prov_apps.ErrUnsupportedPlatform, http.StatusBadRequest},
	"ErrModelNotLocal":            {prov_apps.ErrModelNotLocal, http.StatusBadRequest},
	"ErrParameterValidation":      {prov_apps.ErrParameterValidation, http.StatusBadRequest},
	"ErrPortOutOfRange":           {prov_apps.ErrPortOutOfRange, http.StatusBadRequest},
	"ErrDangerousEnvVar":          {prov_apps.ErrDangerousEnvVar, http.StatusBadRequest},
	"ErrShutdown":                 {prov_apps.ErrShutdown, http.StatusServiceUnavailable},
	"ErrAtCapacity":               {prov_apps.ErrAtCapacity, http.StatusServiceUnavailable},
	"ErrPortUnavailable":          {prov_apps.ErrPortUnavailable, http.StatusServiceUnavailable},
}

// The sentinels reach handlers wrapped in whatever context the raise
// site added, so classification has to survive wrapping -- which is the
// half that a test comparing bare values would not check.
func TestProviderSentinels_MapToTheirDecidedStatus(t *testing.T) {
	for name, tc := range sentinelStatus {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("failed to launch instance: %w",
				fmt.Errorf("build command: %w", tc.err))
			if got := extractStatusCode(wrapped); got != tc.want {
				t.Errorf("%s => %d, want %d", name, got, tc.want)
			}
		})
	}
}

// The table above is only a decision record if it is complete. This
// walks prov_apps/errors.go -- the single place that file's own comment
// declares the root-level error surface lives -- and holds the two in
// step in both directions.
func TestProviderSentinels_EveryExportedErrorHasARow(t *testing.T) {
	const errorsFile = "../../pkg/prov_apps/errors.go"

	f, err := parser.ParseFile(token.NewFileSet(), errorsFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", errorsFile, err)
	}

	declared := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, id := range spec.Names {
			if strings.HasPrefix(id.Name, "Err") {
				declared[id.Name] = true
			}
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatalf("found no sentinels in %s: this guard is not looking where it thinks", errorsFile)
	}

	for name := range declared {
		if _, ok := sentinelStatus[name]; !ok {
			t.Errorf("prov_apps.%s has no row in sentinelStatus: decide its HTTP status, "+
				"or it will be reported as a 500 the caller cannot act on", name)
		}
	}
	for name := range sentinelStatus {
		if !declared[name] {
			t.Errorf("sentinelStatus names prov_apps.%s, which %s no longer declares", name, errorsFile)
		}
	}
}
