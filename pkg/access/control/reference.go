package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"os"

	"github.com/stperic/zzrouter/pkg/access/keys"
)

// CredentialReference binds a delegated grant to one version of an existing key.
// It contains no reusable API credential.
type CredentialReference struct {
	IsVirtual   bool     `json:"is_virtual"`
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Role        UserRole `json:"role"`
	Fingerprint string   `json:"fingerprint"`
}

func credentialVersion(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

func virtualVersion(k *keys.VirtualKey) string {
	return credentialVersion(k.UUID + "\x00" + k.HashedKey + "\x00" + os.Getenv(k.KeyEnv))
}

// Reference authenticates a browser login and binds it to the current credential.
func (c *Core) Reference(raw string) (CredentialReference, error) {
	ac, err := c.Authenticate(raw)
	if err != nil {
		return CredentialReference{}, err
	}
	p := ac.Key
	ref := CredentialReference{ID: p.ID, Role: p.Role, Fingerprint: p.Fingerprint, IsVirtual: p.IsVirtual}
	if !p.IsVirtual {
		ref.Version = credentialVersion(raw)
	} else {
		k := c.keys.Get(p.ID)
		if k == nil || !(k.HashedKey != "" && keys.VerifyKey(raw, k.HashedKey) || k.KeyEnv != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(os.Getenv(k.KeyEnv))) == 1) {
			return CredentialReference{}, ErrInvalidKey
		}
		ref.Version = virtualVersion(k)
	}
	_, err = c.AuthenticateReference(ref)
	return ref, err
}

// AuthenticateReference rechecks live key and team policy for a delegated grant.
func (c *Core) AuthenticateReference(ref CredentialReference) (*AccessContext, error) {
	var p *KeyPrincipal
	var version string
	switch {
	case !ref.IsVirtual && ref.ID == "admin":
		if c.authenticator.staticKeys.Admin != "" {
			p = c.authenticator.tryStaticKey(c.authenticator.staticKeys.Admin)
			version = credentialVersion(c.authenticator.staticKeys.Admin)
		}
	case !ref.IsVirtual && ref.ID == "user":
		if c.authenticator.staticKeys.User != "" {
			p = c.authenticator.tryStaticKey(c.authenticator.staticKeys.User)
			version = credentialVersion(c.authenticator.staticKeys.User)
		}
	default:
		if ref.IsVirtual && c.keys != nil {
			if k := c.keys.Get(ref.ID); k != nil {
				version = virtualVersion(k)
				p = keyPrincipalFromVirtual(k.ID, k, "")
				p.Fingerprint = ref.Fingerprint
			}
		}
	}
	if p == nil || p.Role != ref.Role || version == "" || subtle.ConstantTimeCompare([]byte(version), []byte(ref.Version)) != 1 {
		return nil, ErrInvalidKey
	}
	if p.Suspended {
		return nil, ErrKeySuspended
	}
	if p.IsExpired() {
		return nil, ErrKeyExpired
	}
	ac := c.buildAccessContext(p)
	if ac.Team != nil && ac.Team.Suspended {
		return nil, ErrKeySuspended
	}
	return ac, nil
}
