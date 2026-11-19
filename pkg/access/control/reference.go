package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
)

// CredentialReference binds a delegated grant to one version of an existing key.
// It contains no reusable API credential.
type CredentialReference struct {
	Kind        IdentityKind `json:"kind,omitempty"`
	IsVirtual   bool         `json:"is_virtual"`
	ID          string       `json:"id"`
	Version     string       `json:"version"`
	Role        UserRole     `json:"role"`
	Fingerprint string       `json:"fingerprint"`
}

func credentialVersion(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

// virtualVersion includes the generation, which changes only when the stored content is replaced.
func virtualVersion(generation string, k *keys.VirtualKey) string {
	return credentialVersion(generation + "\x00" + k.UUID + "\x00" + k.HashedKey)
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
		snap := access.SnapshotOf(c.state)
		k := snap.Key(p.ID)
		if k == nil || !keys.VerifyKey(raw, k.HashedKey) {
			return CredentialReference{}, ErrInvalidKey
		}
		ref.Version = virtualVersion(snap.Generation(), k)
	}
	_, err = c.AuthenticateReference(ref)
	return ref, err
}

// AuthenticateReference rechecks live key and team policy for a delegated grant.
func (c *Core) AuthenticateReference(ref CredentialReference) (*AccessContext, error) {
	var p *KeyPrincipal
	var version string
	if ref.Kind != "" && ref.Kind != KindPerson {
		return nil, ErrInvalidKey
	}
	switch {
	case ref.Kind == KindPerson:
		if ref.IsVirtual {
			return nil, ErrInvalidKey
		}
		if person := access.SnapshotOf(c.state).Person(ref.ID); person != nil {
			p = &KeyPrincipal{Kind: KindPerson, ID: person.ID, Name: person.Name, Role: UserRole(person.Role), TeamID: person.TeamID, TeamRole: "member", Suspended: person.Suspended}
			if person.Owner {
				p.TeamRole = "owner"
			}
			version = person.Revision
		}
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
		if ref.IsVirtual {
			snap := access.SnapshotOf(c.state)
			if k := snap.Key(ref.ID); k != nil {
				version = virtualVersion(snap.Generation(), k)
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
	ac, err := c.buildAccessContext(p)
	if err != nil {
		return nil, err
	}
	if ac.Team != nil && ac.Team.Suspended {
		return nil, ErrKeySuspended
	}
	return ac, nil
}

// PersonReference binds the exact verified snapshot, refusing concurrent resets.
func (c *Core) PersonReference(person *people.Person) (CredentialReference, error) {
	if person == nil {
		return CredentialReference{}, ErrInvalidKey
	}
	ref := CredentialReference{Kind: KindPerson, ID: person.ID, Version: person.Revision, Role: UserRole(person.Role)}
	_, err := c.AuthenticateReference(ref)
	return ref, err
}
