package server

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// sentinels names every error prov_apps exports at the HTTP boundary, so
// the guards below can hold failureTable (the decision record) complete
// against pkg/prov_apps/errors.go. Statuses live only in failureTable.
var sentinels = map[string]error{
	"ErrProviderNotFound":         prov_apps.ErrProviderNotFound,
	"ErrInstanceNotFound":         prov_apps.ErrInstanceNotFound,
	"ErrInstallInProgress":        prov_apps.ErrInstallInProgress,
	"ErrExternalService":          prov_apps.ErrExternalService,
	"ErrServiceDisabled":          prov_apps.ErrServiceDisabled,
	"ErrInstancesRunning":         prov_apps.ErrInstancesRunning,
	"ErrProviderNotManaged":       prov_apps.ErrProviderNotManaged,
	"ErrProviderAlreadyInstalled": prov_apps.ErrProviderAlreadyInstalled,
	"ErrInstanceAlreadyExists":    prov_apps.ErrInstanceAlreadyExists,
	"ErrModelAlreadyLoaded":       prov_apps.ErrModelAlreadyLoaded,
	"ErrPortConflict":             prov_apps.ErrPortConflict,
	"ErrModelIncomplete":          prov_apps.ErrModelIncomplete,
	"ErrUnsupportedPlatform":      prov_apps.ErrUnsupportedPlatform,
	"ErrRunHeldByOtherProvider":   prov_apps.ErrRunHeldByOtherProvider,
	"ErrModelNotLocal":            prov_apps.ErrModelNotLocal,
	"ErrParameterValidation":      prov_apps.ErrParameterValidation,
	"ErrPortOutOfRange":           prov_apps.ErrPortOutOfRange,
	"ErrDangerousEnvVar":          prov_apps.ErrDangerousEnvVar,
	"ErrShutdown":                 prov_apps.ErrShutdown,
	"ErrAtCapacity":               prov_apps.ErrAtCapacity,
	"ErrPortUnavailable":          prov_apps.ErrPortUnavailable,
}

// The sentinels reach handlers wrapped in whatever context the raise
// site added, so classification has to survive wrapping -- which is the
// half that a test comparing bare values would not check. A sentinel
// with no row would fall to 500, a failure the caller cannot act on.
func TestProviderSentinels_HaveARowInTheFailureTable(t *testing.T) {
	for name, sentinel := range sentinels {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("failed to launch instance: %w",
				fmt.Errorf("build command: %w", sentinel))
			wire, ok := knownFailure(wrapped)
			require.True(t, ok, "prov_apps.%s has no row in failureTable", name)
			assert.NotEqual(t, http.StatusInternalServerError, wire.Status)
			assert.Equal(t, wire.Status, answeredStatus(wrapped))
		})
	}
}

// failureTable is only a decision record if it is complete. This
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
		if _, ok := sentinels[name]; !ok {
			t.Errorf("prov_apps.%s is not in sentinels: add it, and decide its HTTP status in failureTable, "+
				"or it will be reported as a 500 the caller cannot act on", name)
		}
	}
	for name := range sentinels {
		if !declared[name] {
			t.Errorf("sentinels names prov_apps.%s, which %s no longer declares", name, errorsFile)
		}
	}
}

func TestAdmissionStatusGuard(t *testing.T) {
	for _, reason := range []error{instance.ErrQueueFull, instance.ErrQueueWaitExpired} {
		admission := &instance.AdmissionError{Reason: reason, RetryAfter: 1500 * time.Millisecond}
		failure, ok := renderAdmissionError(fmt.Errorf("wrapped: %w", admission))
		require.True(t, ok)
		assert.Equal(t, http.StatusServiceUnavailable, failure.Status)
		assert.Equal(t, admission.RetryAfter, failure.RetryAfter)
		w := httptest.NewRecorder()
		req := routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		failBeforeBackend(w, req, failure)
		assert.Equal(t, "2", w.Header().Get("Retry-After"))
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, instance.ErrAtCapacity} {
		_, ok := renderAdmissionError(err)
		assert.False(t, ok)
	}
}
