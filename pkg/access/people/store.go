package people

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	maxPeople = 10000
	maxGrants = 4096
)

// PersonRecord is a stored account with its password hash.
type PersonRecord struct {
	Person
	PasswordHash string
}

// GrantRecord is a stored grant with its issuer and browser binding.
type GrantRecord struct {
	Grant
	Issuer      Issuer
	BrowserHash string
}

// State is the people records, grants and onboarding progress. A published
// *State is never changed; a nil *State reads as empty.
type State struct {
	initialized bool
	ownerID     string
	people      map[string]PersonRecord
	grants      map[string]GrantRecord
	progress    Progress
}

// Records is a State's plain transfer form, what its owner stores: people by
// ID, grants by token digest.
type Records struct {
	Initialized bool
	OwnerID     string
	People      map[string]PersonRecord
	Grants      map[string]GrantRecord
	Progress    Progress
}

// Clone is the private copy a transition changes; a nil State clones as empty.
func (s *State) Clone() *State {
	if s == nil {
		return &State{people: make(map[string]PersonRecord), grants: make(map[string]GrantRecord)}
	}
	next := *s
	next.people = maps.Clone(s.people)
	next.grants = maps.Clone(s.grants)
	return &next
}

// NewState validates r and builds a State that shares nothing with it.
func NewState(r Records) (*State, error) {
	state := &State{initialized: r.Initialized, ownerID: r.OwnerID, people: make(map[string]PersonRecord, len(r.People)), grants: make(map[string]GrantRecord, len(r.Grants)), progress: r.Progress}
	maps.Copy(state.people, r.People)
	for hash, grant := range r.Grants {
		grant.Grant = grant.copyTimes()
		state.grants[hash] = grant
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	return state, nil
}

// Records returns a copy of the state's records; a nil State has none.
func (s *State) Records() Records {
	s = s.Clone()
	for hash, grant := range s.grants {
		grant.Grant = grant.copyTimes()
		s.grants[hash] = grant
	}
	return Records{Initialized: s.initialized, OwnerID: s.ownerID, People: s.people, Grants: s.grants, Progress: s.progress}
}

func (s *State) validate() error {
	if len(s.people) > maxPeople || len(s.grants) > maxGrants {
		return fmt.Errorf("invalid people snapshot structure")
	}
	if s.initialized != (s.ownerID != "") {
		return fmt.Errorf("invalid people ownership marker")
	}
	owners := 0
	usernames := make(map[string]bool, len(s.people))
	for id, record := range s.people {
		p := record.Person
		if id != p.ID || validatePerson(p) != nil || usernames[p.Username] || !validPasswordHash(record.PasswordHash) {
			return fmt.Errorf("invalid people snapshot account")
		}
		usernames[p.Username] = true
		if p.Owner {
			owners++
			if id != s.ownerID || p.Role != "admin" || p.Suspended {
				return fmt.Errorf("invalid owner account")
			}
		}
	}
	if s.initialized && owners != 1 || !s.initialized && len(s.people) != 0 {
		return fmt.Errorf("invalid people ownership state")
	}
	for hash, grant := range s.grants {
		if len(hash) != 64 || grant.ID == "" || !validPurpose(grant.Purpose) || grant.ExpiresAt.IsZero() || !validIssuer(grant.Issuer) {
			return fmt.Errorf("invalid people grant snapshot")
		}
	}
	return nil
}

// Store runs people transitions over state its owner persists. It keeps only
// runtime guards: login throttling and Argon2 admission.
type Store struct {
	options   Options
	passwords chan struct{}
	mu        sync.Mutex // guards attempts
	attempts  map[string]loginAttempt
}

// New builds the transitions over the owner's Mutate and View.
func New(options Options) (*Store, error) {
	if options.Mutate == nil || options.View == nil || options.ValidateExternalIssuer == nil {
		return nil, fmt.Errorf("people requires state access and an issuer validator")
	}
	return &Store{options: options, passwords: make(chan struct{}, 2), attempts: make(map[string]loginAttempt)}, nil
}

// view reads the owner's current state and its team check; none reads as empty.
func (s *Store) view() (*State, TeamCheck) {
	state, teams := s.options.View()
	if state == nil {
		state = &State{}
	}
	return state, teams
}

// Initialized reports the permanent owner-claim marker.
func (s *State) Initialized() bool { return s != nil && s.initialized }

// Person returns a copy, or nil for an absent account.
func (s *State) Person(id string) *Person {
	if s == nil {
		return nil
	}
	record, ok := s.people[id]
	if !ok {
		return nil
	}
	p := record.Person
	return &p
}

// Owner returns the immutable initial owner, if claimed.
func (s *State) Owner() *Person {
	if !s.Initialized() {
		return nil
	}
	return s.Person(s.ownerID)
}

// All yields every public account, in no particular order.
func (s *State) All() iter.Seq[Person] {
	return func(yield func(Person) bool) {
		if s == nil {
			return
		}
		for _, record := range s.people {
			if !yield(record.Person) {
				return
			}
		}
	}
}

// People returns public accounts ordered by username.
func (s *State) People() []Person {
	items := slices.AppendSeq([]Person{}, s.All())
	slices.SortFunc(items, func(a, b Person) int { return strings.Compare(a.Username, b.Username) })
	return items
}

// TeamDependencies is the durable membership and pending enrollment impact.
// Suspended people still belong to their team. Pending grants are counted by
// lifecycle, even if their issuer can no longer authorize redemption.
type TeamDependencies struct {
	People           int  `json:"people"`
	Invitations      int  `json:"invitations"`
	EnrollmentGrants int  `json:"enrollment_grants"`
	OwnerProtected   bool `json:"owner_protected"`
}

// HasMembers reports whether stored people or pending grants depend on this team.
func (d TeamDependencies) HasMembers() bool {
	return d.People+d.Invitations+d.EnrollmentGrants > 0
}

// TeamImpact is the canonical projection used by both access UI and deletion.
func (s *State) TeamImpact(id string) TeamDependencies {
	var result TeamDependencies
	if s == nil {
		return result
	}
	for _, record := range s.people {
		if record.TeamID == id {
			result.People++
			result.OwnerProtected = result.OwnerProtected || record.Owner
		}
	}
	for _, grant := range s.grants {
		if grant.TeamID == id && active(grant.Grant) {
			if grant.Purpose == PurposeInvitation {
				result.Invitations++
			} else {
				result.EnrollmentGrants++
			}
		}
	}
	return result
}

// Progress returns the persisted onboarding state.
func (s *State) Progress() Progress {
	if s == nil {
		return Progress{}
	}
	return s.progress
}

func randomValue(prefix string) string {
	return prefix + base64.RawURLEncoding.EncodeToString(randomBytes(32))
}
func randomBytes(size int) []byte { b := make([]byte, size); _, _ = rand.Read(b); return b }
func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func normalizeUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 64 {
		return "", fmt.Errorf("%w: username must contain 3 to 64 letters, digits, dots, underscores or hyphens", ErrInvalidInput)
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-') {
			return "", fmt.Errorf("%w: username must contain letters, digits, dots, underscores or hyphens", ErrInvalidInput)
		}
	}
	return value, nil
}

func validatePerson(p Person) error {
	username, err := normalizeUsername(p.Username)
	if err != nil || username != p.Username || p.ID == "" || p.Revision == "" || !validRole(p.Role) || p.TeamID == "" || !validName(p.Name) {
		return ErrInvalidInput
	}
	return nil
}

func validName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 120
}
func validRole(role string) bool { return role == "admin" || role == "user" }
func validPurpose(p Purpose) bool {
	return p == PurposeSetup || p == PurposeSession || p == PurposeInvitation || p == PurposeRecovery
}
func active(grant Grant) bool {
	return grant.UsedAt == nil && grant.RevokedAt == nil && utils.NowUTC().Before(grant.ExpiresAt)
}

// copyTimes gives g time pointers of its own, so no two holders share one.
func (g Grant) copyTimes() Grant {
	if g.UsedAt != nil {
		value := *g.UsedAt
		g.UsedAt = &value
	}
	if g.RevokedAt != nil {
		value := *g.RevokedAt
		g.RevokedAt = &value
	}
	return g
}

func publicGrant(record GrantRecord) Grant {
	g := record.copyTimes()
	switch {
	case g.UsedAt != nil:
		g.Status = "accepted"
	case g.RevokedAt != nil:
		g.Status = "revoked"
	case !utils.NowUTC().Before(g.ExpiresAt):
		g.Status = "expired"
	default:
		g.Status = "pending"
	}
	return g
}

// UpdateProgress saves navigation choices without accepting success evidence.
func (s *Store) UpdateProgress(ctx context.Context, update ProgressUpdate) error {
	_, err := s.options.Mutate(ctx, func(state *State, _ TeamCheck) (audit.Event, error) {
		if update.Step != nil {
			switch *update.Step {
			case "", "owner", "provider", "model", "test", "app", "complete":
				state.progress.Step = *update.Step
			default:
				return audit.Event{}, ErrInvalidInput
			}
		}
		if update.Dismissed != nil {
			state.progress.Dismissed = *update.Dismissed
		}
		state.progress.UpdatedAt = utils.NowUTC()
		return audit.Event{}, nil
	})
	return err
}

// MarkResponse records successful routed inference by the initial owner.
func (s *Store) MarkResponse(ctx context.Context, personID string) error {
	_, err := s.options.Mutate(ctx, func(state *State, _ TeamCheck) (audit.Event, error) {
		if !state.initialized || state.ownerID != personID {
			return audit.Event{}, ErrNotFound
		}
		state.progress.ResponseVerified = true
		state.progress.UpdatedAt = utils.NowUTC()
		return audit.Event{}, nil
	})
	return err
}

// MarkAppConnected records a successful inference request from an application key.
func (s *Store) MarkAppConnected(ctx context.Context) error {
	_, err := s.options.Mutate(ctx, func(state *State, _ TeamCheck) (audit.Event, error) {
		state.progress.AppConnected = true
		state.progress.UpdatedAt = utils.NowUTC()
		return audit.Event{}, nil
	})
	return err
}
