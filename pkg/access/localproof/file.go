// Package localproof owns the private installation proof shared by the node and local clients.
package localproof

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/security"
)

// Header carries the proof; a URL or body would leak it to logs and change a replayed request's hash.
const Header = "X-Zzrouter-Local-Proof"

// File is the private daemon-created proof consumed by a local operator.
// It must never be returned by an HTTP API or printed by a client.
type File struct {
	Version    int    `json:"version"`
	Proof      string `json:"proof"`
	Endpoint   string `json:"endpoint"`
	ConsoleURL string `json:"console_url"`
	TLSCert    string `json:"tls_cert,omitempty"`
	TLSName    string `json:"tls_name,omitempty"`
}

// Proof owns this daemon lifetime's local recovery authority.
type Proof struct {
	path   string
	digest string
}

// Path is resolved only beneath the operator-selected installation.
func Path(configDir string) string {
	return filepath.Join(configDir, "access", "owner-recovery.json")
}

// Address accepts literal addresses assigned to this host only.
// It avoids DNS or proxy configuration turning a private proof into remote input.
func Address(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok && ip.Equal(network.IP) {
			return true
		}
	}
	return false
}

// New rotates proof only while the caller holds the open access store.
// The containing directory is restricted before any secret bytes are written.
func New(ctx context.Context, configDir string, descriptor File) (*Proof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := Path(configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := security.RestrictDirectoryPermissions(filepath.Dir(path)); err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	descriptor.Version = 1
	descriptor.Proof = base64.RawURLEncoding.EncodeToString(raw)
	data, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("save local recovery authority: %w", err)
	}
	if err := security.RestrictFilePermissions(path); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &Proof{path: path, digest: recoveryDigest(descriptor.Proof)}, nil
}

func recoveryDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Verify compares a supplied proof without retaining its reusable value.
func (r *Proof) Verify(proof string) bool {
	return r != nil && len(proof) == 43 && subtle.ConstantTimeCompare([]byte(r.digest), []byte(recoveryDigest(proof))) == 1
}

// Reference identifies this local authority epoch without disclosing its proof.
func (r *Proof) Reference() string { return "local-recovery:" + r.digest }

// ValidReference rejects grants from a previous daemon lifetime.
func (r *Proof) ValidReference(value string) bool {
	return r != nil && subtle.ConstantTimeCompare([]byte(value), []byte(r.Reference())) == 1
}

// Close removes only this daemon's own proof file, preserving a replacement.
func (r *Proof) Close() error {
	if r == nil {
		return nil
	}
	file, err := readRecoveryPath(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.Verify(file.Proof) {
		return nil
	}
	return os.Remove(r.path)
}

// Read requires the operator to have read access to the daemon's
// protected file. System service installations may require elevation.
func Read(ctx context.Context, configDir string) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := readRecoveryPath(Path(configDir))
	if err != nil {
		return nil, fmt.Errorf("read local recovery authority (run as the service account or administrator for a system installation): %w", err)
	}
	return file, nil
}

func readRecoveryPath(path string) (*File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("local recovery authority is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var file File
	decoder := json.NewDecoder(io.LimitReader(f, 8193))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, errors.New("local recovery authority is unreadable")
	}
	if err := decoder.Decode(new(any)); err != io.EOF || file.Version != 1 || len(file.Proof) != 43 || info.Size() > 8192 {
		return nil, errors.New("local recovery authority is invalid")
	}
	return &file, nil
}
