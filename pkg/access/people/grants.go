package people

import (
	"context"
	"crypto/subtle"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

func validIssuer(issuer Issuer) bool {
	return issuer.Reference != "" && len(issuer.Reference) <= 4096 && (issuer.PersonID == "") == (issuer.Revision == "")
}

func (s *Store) validateIssuer(ctx context.Context, state *State, issuer Issuer, purpose Purpose) error {
	if !validIssuer(issuer) {
		return ErrInvalidGrant
	}
	if issuer.PersonID == "" {
		if err := s.options.ValidateExternalIssuer(ctx, issuer.Reference); err != nil {
			return ErrInvalidGrant
		}
		return nil
	}
	p, ok := state.people[issuer.PersonID]
	if !ok || p.Suspended || p.Revision != issuer.Revision {
		return ErrInvalidGrant
	}
	if purpose != PurposeSession && p.Role != "admin" {
		return ErrInvalidGrant
	}
	if err := s.options.ValidateTeam(ctx, p.TeamID); err != nil {
		return ErrInvalidGrant
	}
	return nil
}

func (s *Store) validateGrant(ctx context.Context, state *State, grant grantRecord) error {
	if !active(grant.Grant) {
		return ErrInvalidGrant
	}
	if err := s.validateIssuer(ctx, state, grant.Issuer, grant.Purpose); err != nil {
		return err
	}
	if grant.TeamID != "" {
		if err := s.options.ValidateTeam(ctx, grant.TeamID); err != nil {
			return ErrInvalidGrant
		}
	}
	if grant.Purpose == PurposeSetup && state.initialized {
		return ErrInitialized
	}
	if grant.Purpose == PurposeRecovery {
		person, ok := state.people[grant.PersonID]
		if !ok || person.TeamID != grant.TeamID {
			return ErrInvalidGrant
		}
	}
	return nil
}

// Issue creates a bounded, expiring grant and persists only its token digest.
func (s *Store) Issue(ctx context.Context, request GrantRequest) (*IssuedGrant, error) {
	if !validPurpose(request.Purpose) || !validIssuer(request.Issuer) {
		return nil, ErrInvalidInput
	}
	if request.TTL == 0 {
		request.TTL = 10 * time.Minute
		if request.Purpose == PurposeInvitation {
			request.TTL = 24 * time.Hour
		}
	}
	maxTTL := 10 * time.Minute
	if request.Purpose == PurposeInvitation {
		maxTTL = 7 * 24 * time.Hour
	}
	if request.TTL <= 0 || request.TTL > maxTTL {
		return nil, ErrInvalidInput
	}
	if request.Purpose != PurposeSession && request.TeamID == "" {
		return nil, ErrInvalidInput
	}
	if request.Purpose == PurposeSetup && (request.Role != "admin" || request.Issuer.PersonID != "") {
		return nil, ErrInvalidInput
	}
	if request.Purpose == PurposeInvitation && !validRole(request.Role) {
		return nil, ErrInvalidInput
	}
	if request.Purpose == PurposeRecovery && request.PersonID == "" {
		return nil, ErrInvalidInput
	}
	raw := randomValue("zzg_")
	now := utils.NowUTC()
	grant := grantRecord{Grant: Grant{ID: randomValue("grant_"), Purpose: request.Purpose, TeamID: request.TeamID, Role: request.Role, PersonID: request.PersonID, CreatedAt: now, ExpiresAt: now.Add(request.TTL)}, Issuer: request.Issuer}
	event := audit.Event{Action: audit.ActionGrantIssued, Subject: grant.ID, Metadata: map[string]any{"purpose": grant.Purpose, "team_id": grant.TeamID, "role": grant.Role}}
	err := s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		if err := s.validateGrant(ctx, state, grant); err != nil {
			return audit.Event{}, err
		}
		ownerRecovery := false
		if request.Purpose == PurposeRecovery {
			person := state.people[request.PersonID]
			if person.Owner && request.Issuer.PersonID != "" {
				return audit.Event{}, ErrOwnerProtected
			}
			ownerRecovery = person.Owner
			// Only the latest recovery operation remains usable for this person.
			invalidateRecoveryGrants(state, person.ID)
		}
		pruneGrants(state, now)
		if !canIssueGrant(state, request.Issuer, ownerRecovery) {
			return audit.Event{}, ErrCapacity
		}
		state.grants[digest(raw)] = grant
		return event, nil
	})
	if err != nil {
		return nil, err
	}
	return &IssuedGrant{Grant: publicGrant(grant), Token: raw}, nil
}

// Retain invitation history for the UI. Used/expired handoff and recovery codes
// need no tombstone: an absent digest already fails redemption.
func pruneGrants(state *State, now time.Time) {
	for hash, grant := range state.grants {
		if now.After(grant.ExpiresAt.Add(24*time.Hour)) || grant.Purpose != PurposeInvitation && !active(grant.Grant) {
			delete(state.grants, hash)
		}
	}
}

func canIssueGrant(state *State, issuer Issuer, ownerRecovery bool) bool {
	// Reserve one slot for the local owner's recovery even when ordinary
	// issuance is saturated. Reissuing recovery replaces its predecessor.
	if ownerRecovery {
		return len(state.grants) < maxGrants
	}
	if len(state.grants) >= maxGrants-1 {
		return false
	}
	const maxActiveGrantsPerIssuer = 64
	count := 0
	for _, grant := range state.grants {
		sameIssuer := grant.Issuer.Reference == issuer.Reference
		if issuer.PersonID != "" {
			sameIssuer = grant.Issuer.PersonID == issuer.PersonID
		}
		if sameIssuer && active(grant.Grant) {
			count++
		}
	}
	return count < maxActiveGrantsPerIssuer
}

// Inspect verifies the token without consuming or exposing its issuer or binding.
func (s *Store) Inspect(ctx context.Context, token string) (*Grant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state := s.view()
	g, ok := state.grants[digest(token)]
	if !ok || len(token) > 256 {
		return nil, ErrInvalidGrant
	}
	if err := s.validateGrant(ctx, state, g); err != nil {
		return nil, err
	}
	result := publicGrant(g)
	return &result, nil
}

// Exchange replaces a bearer code with a proof-bound token in one durable mutation.
func (s *Store) Exchange(ctx context.Context, token, browserProof string) (*IssuedGrant, error) {
	if browserProof == "" || len(browserProof) > 4096 || len(token) > 256 {
		return nil, ErrInvalidGrant
	}
	raw := randomValue("zzb_")
	var result Grant
	err := s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		oldHash := digest(token)
		grant, ok := state.grants[oldHash]
		if !ok || grant.BrowserHash != "" {
			return audit.Event{}, ErrInvalidGrant
		}
		if err := s.validateGrant(ctx, state, grant); err != nil {
			return audit.Event{}, err
		}
		grant.BrowserHash = digest(browserProof)
		delete(state.grants, oldHash)
		state.grants[digest(raw)] = grant
		result = publicGrant(grant)
		return audit.Event{}, nil
	})
	if err != nil {
		return nil, err
	}
	return &IssuedGrant{Grant: result, Token: raw}, nil
}

func (s *Store) redeem(ctx context.Context, state *State, token, proof string, purpose Purpose) (grantRecord, error) {
	grant, ok := state.grants[digest(token)]
	if !ok || grant.Purpose != purpose || grant.BrowserHash == "" || len(token) > 256 || proof == "" || len(proof) > 4096 || subtle.ConstantTimeCompare([]byte(grant.BrowserHash), []byte(digest(proof))) != 1 {
		return grantRecord{}, ErrInvalidGrant
	}
	if err := s.validateGrant(ctx, state, grant); err != nil {
		return grantRecord{}, err
	}
	now := utils.NowUTC()
	grant.UsedAt = &now
	state.grants[digest(token)] = grant
	return grant, nil
}

// ConsumeSession transfers exactly the issuer's authority, never an owner's identity.
func (s *Store) ConsumeSession(ctx context.Context, token, proof string) (Issuer, error) {
	var issuer Issuer
	err := s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		grant, err := s.redeem(ctx, state, token, proof, PurposeSession)
		if err != nil {
			return audit.Event{}, err
		}
		issuer = grant.Issuer
		return audit.Event{}, nil
	})
	return issuer, err
}

// ClaimOwner atomically creates the initial account, closes setup, and consumes its grant.
func (s *Store) ClaimOwner(ctx context.Context, token, proof string, input CreatePerson) (*Person, error) {
	return s.enroll(ctx, token, proof, input, PurposeSetup)
}

// AcceptInvitation creates one account with the grant's fixed team and role.
func (s *Store) AcceptInvitation(ctx context.Context, token, proof string, input CreatePerson) (*Person, error) {
	return s.enroll(ctx, token, proof, input, PurposeInvitation)
}

func (s *Store) enroll(ctx context.Context, token, proof string, input CreatePerson, purpose Purpose) (*Person, error) {
	username, err := normalizeUsername(input.Username)
	if err != nil {
		return nil, err
	}
	if !validName(input.Name) {
		return nil, fmt.Errorf("%w: a display name of up to 120 characters is required", ErrInvalidInput)
	}
	// Reject obviously invalid grants before expensive password work, then recheck
	// authority and single-use state in the commit transaction below.
	if _, err := s.Inspect(ctx, token); err != nil {
		return nil, err
	}
	hash, err := s.hashPassword(ctx, input.Password)
	if err != nil {
		return nil, err
	}
	var result Person
	var event audit.Event
	err = s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		if len(state.people) >= maxPeople {
			return audit.Event{}, ErrCapacity
		}
		for _, p := range state.people {
			if p.Username == username {
				return audit.Event{}, ErrConflict
			}
		}
		grant, err := s.redeem(ctx, state, token, proof, purpose)
		if err != nil {
			return audit.Event{}, err
		}
		if purpose == PurposeInvitation && !state.initialized {
			return audit.Event{}, ErrInvalidGrant
		}
		now := utils.NowUTC()
		result = Person{ID: randomValue("person_"), Username: username, Name: strings.TrimSpace(input.Name), Role: grant.Role, TeamID: grant.TeamID, Owner: purpose == PurposeSetup, Revision: randomValue(""), CreatedAt: now, UpdatedAt: now}
		state.people[result.ID] = personRecord{Person: result, PasswordHash: hash}
		if purpose == PurposeSetup {
			state.initialized = true
			state.ownerID = result.ID
			state.progress.Step = "provider"
			state.progress.UpdatedAt = now
		}
		event = audit.Event{Action: audit.ActionPersonCreated, Actor: result.ID, Subject: result.ID, Metadata: map[string]any{"purpose": purpose, "team_id": result.TeamID, "role": result.Role, "grant_id": grant.ID}}
		return event, nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Recover consumes a recovery grant and invalidates the person's previous authority.
func (s *Store) Recover(ctx context.Context, token, proof, password string) (*Person, error) {
	if _, err := s.Inspect(ctx, token); err != nil {
		return nil, err
	}
	hash, err := s.hashPassword(ctx, password)
	if err != nil {
		return nil, err
	}
	var result Person
	var event audit.Event
	err = s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		grant, err := s.redeem(ctx, state, token, proof, PurposeRecovery)
		if err != nil {
			return audit.Event{}, err
		}
		record, ok := state.people[grant.PersonID]
		if !ok {
			return audit.Event{}, ErrInvalidGrant
		}
		record.PasswordHash = hash
		record.Revision = randomValue("")
		record.UpdatedAt = utils.NowUTC()
		state.people[record.ID] = record
		invalidatePersonGrants(state, record.ID)
		result = record.Person
		event = audit.Event{Action: audit.ActionPersonRecovered, Actor: record.ID, Subject: record.ID, Metadata: map[string]any{"grant_id": grant.ID, "authority_invalidated": true}}
		return event, nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func invalidatePersonGrants(state *State, id string) {
	now := utils.NowUTC()
	for hash, grant := range state.grants {
		if active(grant.Grant) && (grant.PersonID == id || grant.Issuer.PersonID == id) {
			grant.RevokedAt = &now
			state.grants[hash] = grant
		}
	}
}

func invalidateRecoveryGrants(state *State, id string) {
	now := utils.NowUTC()
	for hash, grant := range state.grants {
		if grant.Purpose == PurposeRecovery && grant.PersonID == id && active(grant.Grant) {
			grant.RevokedAt = &now
			state.grants[hash] = grant
		}
	}
}

// Invitations returns safe invitation/recovery history, including expired and consumed records.
func (s *State) Invitations() []Grant {
	items := []Grant{}
	if s == nil {
		return items
	}
	for _, grant := range s.grants {
		if grant.Purpose == PurposeInvitation || grant.Purpose == PurposeRecovery {
			items = append(items, publicGrant(grant))
		}
	}
	slices.SortFunc(items, func(a, b Grant) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return items
}

// RevokeGrant invalidates a pending grant by its public identifier.
func (s *Store) RevokeGrant(ctx context.Context, id string) error {
	var event audit.Event
	return s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		for hash, grant := range state.grants {
			if grant.ID == id {
				if active(grant.Grant) {
					now := utils.NowUTC()
					grant.RevokedAt = &now
					state.grants[hash] = grant
					event = audit.Event{Action: audit.ActionGrantRevoked, Subject: id, Metadata: map[string]any{"purpose": grant.Purpose, "team_id": grant.TeamID}}
				}
				return event, nil
			}
		}
		return audit.Event{}, ErrNotFound
	})
}

// Update changes mutable properties and revokes earlier sessions when authority changes.
func (s *Store) Update(ctx context.Context, id string, update UpdatePerson) (*Person, error) {
	var result Person
	var event audit.Event
	err := s.options.Mutate(ctx, func(state *State) (audit.Event, error) {
		record, ok := state.people[id]
		if !ok {
			return audit.Event{}, ErrNotFound
		}
		if record.Owner && (update.Role != nil && *update.Role != record.Role || update.TeamID != nil && *update.TeamID != record.TeamID || update.Suspended != nil && *update.Suspended) {
			return audit.Event{}, ErrOwnerProtected
		}
		authorityChanged := false
		if update.Name != nil {
			if !validName(*update.Name) {
				return audit.Event{}, ErrInvalidInput
			}
			record.Name = strings.TrimSpace(*update.Name)
		}
		if update.Role != nil {
			if !validRole(*update.Role) {
				return audit.Event{}, ErrInvalidInput
			}
			authorityChanged = authorityChanged || record.Role != *update.Role
			record.Role = *update.Role
		}
		if update.TeamID != nil {
			if *update.TeamID == "" {
				return audit.Event{}, ErrInvalidInput
			}
			if err := s.options.ValidateTeam(ctx, *update.TeamID); err != nil {
				return audit.Event{}, err
			}
			authorityChanged = authorityChanged || record.TeamID != *update.TeamID
			record.TeamID = *update.TeamID
		}
		if update.Suspended != nil {
			authorityChanged = authorityChanged || record.Suspended != *update.Suspended
			record.Suspended = *update.Suspended
		}
		if authorityChanged {
			record.Revision = randomValue("")
			invalidatePersonGrants(state, id)
		}
		if authorityChanged || record.Name != state.people[id].Name {
			event = audit.Event{Action: audit.ActionPersonUpdated, Subject: id, Metadata: map[string]any{"name_changed": record.Name != state.people[id].Name, "team_id": record.TeamID, "role": record.Role, "suspended": record.Suspended, "authority_invalidated": authorityChanged}}
		}
		record.UpdatedAt = utils.NowUTC()
		state.people[id] = record
		result = record.Person
		return event, nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
