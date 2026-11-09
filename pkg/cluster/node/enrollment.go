package clusternode

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

const enrollmentPath = "/zzrouter/v1/cluster/enrollment"
const enrollmentCodeHeader = "X-ZZRouter-Pairing-Code"

func (n *Node) windowInfo(w *pairingWindow) PairingWindowInfo {
	info := PairingWindowInfo{Code: w.code, Deadline: w.deadline, Fingerprint: n.Fingerprint()}
	if w.listener != nil {
		host := ResolveSelfHost(n.cfg.BindHost, n.cfg.NodeName, n.cfg.advertiseIPs())
		_, port, _ := net.SplitHostPort(w.listener.Addr().String())
		info.WorkerURL = "https://" + net.JoinHostPort(host, port)
	}
	return info
}
func (n *Node) serveEnrollment(w *pairingWindow) {
	defer n.wg.Done()
	defer close(w.done)
	served := make(chan struct{})
	go func() { defer close(served); _ = w.server.ServeTLS(w.listener, "", "") }()
	<-w.ctx.Done()
	if n.Mode() != Worker {
		_ = w.server.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.server.Shutdown(ctx); err != nil {
		_ = w.server.Close()
	}
	<-served
	n.pairingMu.Lock()
	if n.activePairingWindow == w {
		n.cancelWindowLocked()
	}
	n.pairingMu.Unlock()
	n.mu.Lock()
	if n.Mode() == Unclaimed && n.listenerAddr == w.listener.Addr() {
		n.listenerAddr = nil
	}
	n.mu.Unlock()
}
func (n *Node) handleEnrollment(w *pairingWindow, rw http.ResponseWriter, r *http.Request) {
	host, _, addrErr := net.SplitHostPort(r.RemoteAddr)
	if addrErr != nil || !n.pairingRateLimiter.Allow(host) {
		http.Error(rw, "enrollment rate limit", http.StatusTooManyRequests)
		return
	}
	if r.URL.Path != enrollmentPath {
		http.NotFound(rw, r)
		return
	}
	n.pairingMu.Lock()
	valid := n.activePairingWindow == w && w.ctx.Err() == nil && utils.Now().Before(w.deadline) && subtle.ConstantTimeCompare([]byte(r.Header.Get(enrollmentCodeHeader)), []byte(w.code)) == 1
	if !valid {
		n.pairingMu.Unlock()
		http.Error(rw, "enrollment window or code is invalid", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		request := w.request
		n.pairingMu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(request)
		return
	}
	if r.Method != http.MethodPost {
		n.pairingMu.Unlock()
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var approved pairingApprovedResponse
	n.pairingMu.Unlock()
	r.Body = http.MaxBytesReader(rw, r.Body, pairingRequestMaxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&approved); err != nil {
		http.Error(rw, "invalid enrollment response", http.StatusBadRequest)
		return
	}
	n.pairingMu.Lock()
	if n.activePairingWindow != w || w.ctx.Err() != nil || !utils.Now().Before(w.deadline) {
		n.pairingMu.Unlock()
		http.Error(rw, "enrollment expired or cancelled", http.StatusForbidden)
		return
	}
	err := n.verifyEnrollment(w, approved)
	if err == nil {
		err = n.installPairingLocked([]byte(approved.SignedCertPEM), []byte(approved.CACertPEM), approved.CoordinatorURL)
	}
	n.pairingMu.Unlock()
	if err != nil {
		http.Error(rw, "enrollment refused: "+err.Error(), http.StatusBadRequest)
		return
	}
	n.pairingInstalled()
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]string{"status": "paired"})
}
func (n *Node) verifyEnrollment(w *pairingWindow, approved pairingApprovedResponse) error {
	if approved.CoordinatorURL != w.options.CoordinatorURL {
		return errors.New("coordinator URL does not match enrollment intent")
	}
	block, _ := pem.Decode([]byte(approved.SignedCertPEM))
	if block == nil {
		return errors.New("missing signed node certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if !samePublicKey(leaf.PublicKey, n.identity.Certificate().PublicKey) || leaf.Subject.CommonName != n.cfg.NodeName || !hasOU(leaf, clusterid.RoleWorker.OU()) {
		return errors.New("signed certificate does not match this worker")
	}
	if err := verifyCAPEMSignsLeaf([]byte(approved.CACertPEM), leaf); err != nil {
		return err
	}
	if w.options.CAFingerprint != "" {
		pin, err := normalizeCAFingerprint(w.options.CAFingerprint)
		if err != nil {
			return err
		}
		if err := verifyCAPEMMatchesPin([]byte(approved.CACertPEM), pin); err != nil {
			return err
		}
	}
	return nil
}

// Enroll initiates pinned enrollment to an explicitly armed worker listener.
func (n *Node) Enroll(ctx context.Context, workerURL, fingerprint, code, coordinatorURL string) (*PairingAcceptance, error) {
	if n.Mode() != Coordinator || n.ca == nil {
		return nil, ErrNotCoordinator
	}
	target, err := url.Parse(workerURL)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("worker_url must be an HTTPS cluster endpoint")
	}
	if !IsValidPairingCode(code) {
		return nil, errors.New("malformed enrollment code")
	}
	pin, err := clusterid.NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	client, err := newCAPinnedClient(pin)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	target.Path = enrollmentPath
	request := func(method string, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
		if err != nil {
			return nil, err
		}
		req.Header.Set(enrollmentCodeHeader, code)
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}
	resp, err := request(http.MethodGet, nil)
	if err != nil {
		// The signed identity is the durable receipt if an earlier delivery committed.
		accepted, confirmErr := n.confirmEnrolled(ctx, target.String(), pin, "")
		if confirmErr == nil {
			return accepted, nil
		}
		return nil, errors.Join(err, confirmErr)
	}
	var pending pairingRequestBody
	err = json.NewDecoder(io.LimitReader(resp.Body, pairingRequestMaxBodyBytes)).Decode(&pending)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker enrollment returned status %d", resp.StatusCode)
	}
	if err != nil {
		return nil, err
	}
	if err := n.validateEnrollmentRequest(pending, resp.TLS.PeerCertificates[0], pin); err != nil {
		return nil, err
	}
	signed, err := n.ca.Sign([]byte(pending.CSRPEM), clusterid.RoleWorker)
	if err != nil {
		return nil, err
	}
	approved := pairingApprovedResponse{Status: "approved", SignedCertPEM: string(signed), CACertPEM: string(n.ca.CertPEM()), CoordinatorURL: coordinatorURL, ClusterProtocol: version.ClusterProtocolVersion, MinClusterProtocol: version.MinClusterProtocolVersion}
	body, err := json.Marshal(approved)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	if n.stopped || n.shutdownCtx == nil {
		n.mu.Unlock()
		return nil, errors.New("coordinator stopped during enrollment")
	}
	deliveryCtx, deliveryCancel := context.WithTimeout(n.shutdownCtx, 45*time.Second)
	n.wg.Add(1)
	n.mu.Unlock()
	defer n.wg.Done()
	defer deliveryCancel()
	// Certificate delivery is a commit: finish bounded identity confirmation even if the operator disconnects.
	ctx = deliveryCtx
	resp, postErr := request(http.MethodPost, bytes.NewReader(body))
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			postErr = fmt.Errorf("worker installation returned status %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	accepted, confirmErr := n.confirmEnrolled(ctx, target.String(), pin, pending.NodeName)
	if confirmErr != nil {
		return nil, errors.Join(postErr, confirmErr)
	}
	return accepted, nil
}

func (n *Node) confirmEnrolled(ctx context.Context, targetURL, pin, expectedName string) (*PairingAcceptance, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := n.DialClient(clusterid.RoleWorker.OU())
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}
	target.Path = "/zzrouter/v1/internal/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confirm enrolled worker: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		return nil, errors.New("enrolled worker protocol is unavailable")
	}
	leaf := response.TLS.PeerCertificates[0]
	if err := n.ValidateWorker(leaf); err != nil {
		return nil, err
	}
	if leaf.Subject.CommonName == "" || (expectedName != "" && leaf.Subject.CommonName != expectedName) || !spkiMatches(leaf, pin) {
		return nil, errors.New("installed worker identity differs from enrollment")
	}
	var info struct {
		Protocol int `json:"cluster_protocol"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&info); err != nil {
		return nil, err
	}
	if err := version.LocalProtocolWindow().Check(info.Protocol); err != nil {
		return nil, err
	}
	fp := clusterid.Fingerprint(leaf)
	return &PairingAcceptance{NodeName: leaf.Subject.CommonName, Fingerprint: fp, ShortForm: clusterid.ShortForm(fp), RemoteAddr: target.Hostname()}, nil
}

func (n *Node) validateEnrollmentRequest(pending pairingRequestBody, peer *x509.Certificate, pin string) error {
	if err := version.LocalProtocolWindow().Check(pending.ClusterProtocol); err != nil {
		return err
	}
	fp, err := clusterid.NormalizeFingerprint(pending.Fingerprint)
	if err != nil || fp != pin {
		return errors.New("worker identity does not match pinned enrollment")
	}
	csr, err := parseCSRPEM([]byte(pending.CSRPEM))
	if err != nil {
		return err
	}
	if csr.Subject.CommonName != pending.NodeName || clusterid.Fingerprint(peer) != pending.Fingerprint || !samePublicKey(csr.PublicKey, peer.PublicKey) {
		return errors.New("worker CSR does not match authenticated enrollment identity")
	}
	if n.cfg.RequireSecurePairing {
		if err := verifyWorkerPinnedOurCA(pending.CoordinatorCAFingerprint, n.CAFingerprint()); err != nil {
			return err
		}
	}
	if n.deny != nil && n.deny.Contains(pending.Fingerprint) {
		return ErrRenewDenyListed
	}
	return nil
}
