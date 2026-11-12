package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stretchr/testify/require"
)

func TestNativeAuthenticationDoesNotWaitForOAuthTransaction(t *testing.T) {
	for _, key := range []string{TestAdminKey, "mcpa_" + TestAdminKey} {
		t.Run(key[:8], func(t *testing.T) {
			core := control.NewCore(control.StaticKeySet{Admin: key}, nil, nil, nil, nil, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			authority, err := oauth.New(oauth.Options[control.CredentialReference]{
				Issuer: "http://127.0.0.1:19090", StatePath: filepath.Join(t.TempDir(), "oauth.json"),
				Resources: []oauth.Resource{{Path: adminMCPPath, Scope: "zzrouter:manage"}},
				Browser:   oauth.BrowserOptions{SessionCookie: browserSessionCookie, CSRFCookie: browserCSRFCookie},
				Authenticate: func(_ context.Context, raw string) (control.CredentialReference, error) {
					return core.Reference(raw)
				},
				Resolve: func(context.Context, control.CredentialReference) (oauth.IdentityView, error) {
					close(entered)
					<-release
					return oauth.IdentityView{ID: "test-admin"}, nil
				},
				Authorize: func(context.Context, control.CredentialReference, oauth.Resource) error { return nil },
				Render:    func(io.Writer, oauth.Page) error { return nil },
			})
			require.NoError(t, err)
			var tasks sync.WaitGroup
			t.Cleanup(func() {
				close(release)
				tasks.Wait()
				require.NoError(t, authority.Close())
			})
			ref, err := core.Reference(key)
			require.NoError(t, err)
			transactionDone := make(chan error, 1)
			tasks.Go(func() {
				_, err := authority.Connections(t.Context(), ref)
				transactionDone <- err
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("OAuth transaction did not acquire its state mutex")
			}
			access := &AccessControl{core: core, oauth: authority}
			for _, path := range []string{"/v1/models", "/api/tags", "/zzrouter/v1/nodes"} {
				result := make(chan error, 1)
				tasks.Go(func() {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodGet, path, nil)
					_, err := access.authenticateCredential(c, credential{key: key, scheme: "X-API-Key"})
					result <- err
				})
				select {
				case err := <-result:
					require.NoError(t, err, path)
				case <-time.After(5 * time.Second):
					t.Fatalf("native authentication for %s waited for the OAuth transaction", path)
				}
				select {
				case <-transactionDone:
					t.Fatal("the OAuth transaction must still hold its mutex when native authentication completes")
				default:
				}
			}
		})
	}
}
