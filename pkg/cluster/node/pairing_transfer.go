package clusternode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

// MaxPairingTransferSize bounds clipboard and file enrollment documents.
const MaxPairingTransferSize = 8 << 10

// PairingTransfer carries one armed worker's enrollment inputs. It conveys no
// trust by itself: the coordinator still pins and verifies the worker's identity.
type PairingTransfer struct {
	Type              string    `json:"type"`
	Version           int       `json:"version"`
	NodeName          string    `json:"node_name"`
	Code              string    `json:"code"`
	WorkerURL         string    `json:"worker_url"`
	WorkerFingerprint string    `json:"worker_fingerprint"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// Transfer returns the portable document for an existing pairing window.
func (p PairingWindowInfo) Transfer() PairingTransfer {
	return PairingTransfer{Type: "zzrouter-pairing", Version: 1, NodeName: p.NodeName,
		Code: p.Code, WorkerURL: p.WorkerURL, WorkerFingerprint: p.Fingerprint, ExpiresAt: p.Deadline}
}

// Validate rejects ambiguous or expired inputs before any network operation.
// Worker-side window expiry remains authoritative even when clocks disagree.
func (p PairingTransfer) Validate(now time.Time) error {
	if p.Type != "zzrouter-pairing" || p.Version != 1 {
		return errors.New("unsupported pairing document type or version")
	}
	if strings.TrimSpace(p.NodeName) == "" || len(p.NodeName) > 255 || strings.IndexFunc(p.NodeName, unicode.IsControl) >= 0 {
		return errors.New("pairing document needs a valid computer name")
	}
	if !IsValidPairingCode(p.Code) {
		return errors.New("malformed pairing code")
	}
	if err := validateEnrollmentURL(p.WorkerURL); err != nil {
		return err
	}
	if _, err := clusterid.NormalizeFingerprint(p.WorkerFingerprint); err != nil {
		return errors.New("malformed worker fingerprint")
	}
	if p.ExpiresAt.IsZero() || !now.Before(p.ExpiresAt) {
		return errors.New("pairing document expired; prepare pairing on the computer again")
	}
	return nil
}

func validateEnrollmentURL(raw string) error {
	target, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" || target.Opaque != "" {
		return errors.New("worker_url must be an HTTPS cluster endpoint")
	}
	return nil
}

// ReadPairingTransfer reads exactly one bounded, strictly decoded document.
func ReadPairingTransfer(r io.Reader, now time.Time) (PairingTransfer, error) {
	var p PairingTransfer
	data, err := io.ReadAll(io.LimitReader(r, MaxPairingTransferSize+1))
	if err != nil {
		return p, fmt.Errorf("read pairing document: %w", err)
	}
	if len(data) > MaxPairingTransferSize {
		return p, errors.New("pairing document exceeds 8 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, errors.New("invalid pairing document")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return p, errors.New("expected one pairing document")
	}
	return p, p.Validate(now)
}
