package clusternode

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorInitiatedEnrollmentAndRenewal(t *testing.T) {
	coord, coordURL := startNode(t, Coordinator)
	worker, _ := startNode(t, Unclaimed)
	window, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL, CAFingerprint: clusterid.Fingerprint(coord.ca.Certificate())})
	require.NoError(t, err)
	require.NotEmpty(t, window.WorkerURL)
	require.NotEmpty(t, window.Fingerprint)
	accepted, err := coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.NoError(t, err)
	require.Equal(t, worker.cfg.NodeName, accepted.NodeName)
	require.Equal(t, Worker, worker.Mode())
	client, err := coord.DialClient(clusterid.RoleWorker.OU())
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, window.WorkerURL+"/health", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()
	csr, err := worker.identity.CSR(worker.cfg.NodeName, worker.cfg.advertiseDNSNames(), worker.cfg.advertiseIPs())
	require.NoError(t, err)
	cert, err := coord.SignRenewal(worker.identity.Certificate(), string(csr))
	require.NoError(t, err)
	require.NoError(t, worker.verifyRenewedCert([]byte(cert)))
	require.NoError(t, worker.Stop(context.Background()))
}
func TestEnrollmentRegenerationClosesPreviousListener(t *testing.T) {
	worker, _ := startNode(t, Unclaimed)
	one, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: "https://coord:9091"})
	require.NoError(t, err)
	two, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: "https://coord:9091", Regenerate: true})
	require.NoError(t, err)
	require.NotEqual(t, one.Code, two.Code)
	require.NoError(t, worker.CancelPairing())
}
func TestEnrollmentRejectsWrongIdentityAndCancelledWindow(t *testing.T) {
	coord, coordURL := startNode(t, Coordinator)
	worker, _ := startNode(t, Unclaimed)
	window, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL})
	require.NoError(t, err)
	_, err = coord.Enroll(t.Context(), window.WorkerURL, clusterid.Fingerprint(coord.ca.Certificate()), window.Code, coordURL)
	require.Error(t, err)
	require.Equal(t, Unclaimed, worker.Mode())
	require.NoError(t, worker.CancelPairing())
	_, err = coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.Error(t, err)
	require.Equal(t, Unclaimed, worker.Mode())
}

func TestEnrollmentRecoversExpiredIdentity(t *testing.T) {
	coord, coordURL := startNode(t, Coordinator)
	worker, _ := startNode(t, Unclaimed)
	window, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL})
	require.NoError(t, err)
	_, err = coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.NoError(t, err)
	fingerprint := worker.Fingerprint()
	leaf := *worker.identity.Certificate()
	leaf.SerialNumber = big.NewInt(987)
	leaf.NotBefore, leaf.NotAfter = utils.Now().Add(-2*time.Hour), utils.Now().Add(-time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, &leaf, &leaf, worker.identity.Signer().Public(), worker.identity.Signer())
	require.NoError(t, err)
	require.NoError(t, worker.identity.InstallSignedCert(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	require.NoError(t, worker.ResetPairing())
	window, err = worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL})
	require.NoError(t, err)
	require.Equal(t, fingerprint, window.Fingerprint)
	_, err = coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.NoError(t, err)
	require.Equal(t, Worker, worker.Mode())
	require.Equal(t, fingerprint, worker.Fingerprint())
}

func TestEnrollmentDeadlineRefusesTrustCommit(t *testing.T) {
	coord, coordURL := startNode(t, Coordinator)
	worker, _ := startNode(t, Unclaimed)
	window, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL})
	require.NoError(t, err)
	worker.pairingMu.Lock()
	worker.activePairingWindow.deadline = utils.Now().Add(-time.Second)
	worker.pairingMu.Unlock()
	_, err = coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.Error(t, err)
	require.Equal(t, Unclaimed, worker.Mode())
}

func TestEnrollmentCommittedDeliveryRetry(t *testing.T) {
	coord, coordURL := startNode(t, Coordinator)
	worker, _ := startNode(t, Unclaimed)
	window, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL})
	require.NoError(t, err)
	one, err := coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.NoError(t, err)
	serial := worker.identity.Certificate().SerialNumber.String()
	two, err := coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.NoError(t, err)
	require.Equal(t, one, two)
	require.Equal(t, serial, worker.identity.Certificate().SerialNumber.String())
}

func TestRenewalAndRevocationUseCoordinatorExchange(t *testing.T) {
	coord, coordURL := startNode(t, Coordinator)
	worker, err := New(testConfig(t, Unclaimed))
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/zzrouter/v1/internal/signals/poll", func(w http.ResponseWriter, r *http.Request) {
		var poll clustersignal.Poll
		if err := json.NewDecoder(r.Body).Decode(&poll); err != nil {
			w.WriteHeader(400)
			return
		}
		batch, err := worker.Signals().Wait(r.Context(), poll)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(batch)
	})
	mux.HandleFunc("/zzrouter/v1/internal/signals/ack", func(w http.ResponseWriter, r *http.Request) {
		var ack clustersignal.Acknowledgment
		if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
			w.WriteHeader(400)
			return
		}
		if !worker.Signals().Acknowledge(ack) {
			w.WriteHeader(409)
		}
	})
	mux.Handle("/", stubInferenceHandler())
	require.NoError(t, worker.SetAdminAPIHandler(mux))
	require.NoError(t, worker.SetWorkerCompatHandler(stubInferenceHandler()))
	require.NoError(t, worker.Start(t.Context()))
	t.Cleanup(func() { _ = worker.Stop(context.Background()) })
	window, err := worker.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: coordURL})
	require.NoError(t, err)
	_, err = coord.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, coordURL)
	require.NoError(t, err)
	client, err := coord.DialClient(clusterid.RoleWorker.OU())
	require.NoError(t, err)
	owner := clustersignal.NewCoordinator(clustersignal.CoordinatorConfig{Client: func() *http.Client { return client }, Member: func(url, node string) bool { return url == window.WorkerURL && node == worker.cfg.NodeName }, Handle: func(_ context.Context, peer *x509.Certificate, op clustersignal.Operation) clustersignal.Result {
		cert, err := coord.SignRenewal(peer, op.CSR)
		if errors.Is(err, ErrRenewDenyListed) {
			return clustersignal.Result{Error: &clustersignal.StatusError{Status: 403, Code: "revoked", Detail: err.Error()}}
		}
		if err != nil {
			return clustersignal.Result{Error: &clustersignal.StatusError{Status: 400, Detail: err.Error()}}
		}
		return clustersignal.Result{Certificate: cert}
	}})
	owner.Start(t.Context())
	t.Cleanup(func() { owner.Stop(context.Background()) })
	owner.Observe(window.WorkerURL, worker.cfg.NodeName)
	before := worker.identity.Certificate().SerialNumber.String()
	require.NoError(t, worker.renewOnce(t.Context()))
	require.NotEqual(t, before, worker.identity.Certificate().SerialNumber.String())
	require.Equal(t, window.Fingerprint, worker.Fingerprint())
	require.NoError(t, coord.Revoke(worker.Fingerprint(), "", "test"))
	require.ErrorIs(t, worker.renewOnce(t.Context()), ErrRenewDenyListed)
	_, err = worker.DialClient(clusterid.RoleCoordinator.OU())
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestEnrollmentURLUsesCanonicalAdvertisedHost(t *testing.T) {
	for _, tc := range []struct{ name, bind, want string }{{"wildcard", "", "192.0.2.10"}, {"concrete bind", "127.0.0.1", "127.0.0.1"}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, Unclaimed)
			cfg.BindHost = tc.bind
			cfg.AdvertiseIPs = []net.IP{net.ParseIP("192.0.2.10")}
			n, err := New(cfg)
			require.NoError(t, err)
			require.NoError(t, n.SetAdminAPIHandler(stubInferenceHandler()))
			require.NoError(t, n.SetWorkerCompatHandler(stubInferenceHandler()))
			require.NoError(t, n.Start(t.Context()))
			t.Cleanup(func() { _ = n.Stop(context.Background()) })
			window, err := n.BeginPairing(t.Context(), BeginPairingOptions{CoordinatorURL: "https://coordinator:9091"})
			require.NoError(t, err)
			endpoint, err := url.Parse(window.WorkerURL)
			require.NoError(t, err)
			require.Equal(t, tc.want, endpoint.Hostname())
		})
	}
}

func TestEnrollmentCancelledAdmissionCannotOpenLateWindow(t *testing.T) {
	worker, _ := startNode(t, Unclaimed)
	ctx, cancel := context.WithCancel(t.Context())
	worker.pairingMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := worker.BeginPairing(ctx, BeginPairingOptions{CoordinatorURL: "https://coordinator:9091"})
		done <- err
	}()
	cancel()
	worker.pairingMu.Unlock()
	require.ErrorIs(t, <-done, context.Canceled)
	_, active := worker.PairingWindowInfoSnapshot()
	require.False(t, active)
	require.Nil(t, worker.Addr())
}
