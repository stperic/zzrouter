package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/localproof"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

// validateExternalIssuer runs inside a people commit, under the access store's writer gate, so it never reads a person.
func (s *Server) validateExternalIssuer(_ context.Context, value string) error {
	if s.ownerRecovery != nil && s.ownerRecovery.ValidReference(value) {
		return nil
	}
	var ref control.CredentialReference
	if err := json.Unmarshal([]byte(value), &ref); err != nil || ref.Kind == control.KindPerson {
		return people.ErrInvalidGrant
	}
	_, err := s.access.core.AuthenticateReference(ref)
	return err
}

// validatePeopleTeam admits people and grants only into an active shared team.
func validatePeopleTeam(snap *access.Snapshot, id string) error {
	team := snap.Team(id)
	if team == nil || team.Kind != teams.KindShared || team.Suspended {
		return people.ErrInvalidGrant
	}
	return nil
}

func (s *Server) initPeople(ctx context.Context, issuer string) error {
	var err error
	descriptor := localproof.File{ConsoleURL: issuer + "/console/", TLSCert: s.config.Node.TLSCert}
	if descriptor.TLSCert != "" {
		descriptor.TLSCert, err = filepath.Abs(descriptor.TLSCert)
		if err != nil {
			return err
		}
	}
	host, err := localRecoveryHost(ctx, s.config.Node.Bind)
	if err != nil {
		return err
	}
	scheme := "http"
	if s.config.Node.IsTLSEnabled() {
		scheme = "https"
		parsed, _ := url.Parse(issuer)
		descriptor.TLSName = parsed.Hostname()
	}
	descriptor.Endpoint = scheme + "://" + net.JoinHostPort(host, strconv.Itoa(s.config.Node.Port))
	s.ownerRecovery, err = localproof.New(ctx, s.configDir, descriptor)
	if err != nil {
		return fmt.Errorf("initialize owner recovery: %w", err)
	}
	if s.inference.logBridge != nil {
		var responseDone, appDone atomic.Bool
		store := s.accessStore.People()
		progress := s.accessSnapshot().Progress()
		responseDone.Store(progress.ResponseVerified)
		appDone.Store(progress.AppConnected)
		s.inference.logBridge.SetOnboardingObserver(func(subject control.Subject) {
			if subject.Kind == control.KindPerson && !responseDone.Load() {
				if err := store.MarkResponse(ctx, subject.ID); err != nil {
					if !errors.Is(err, people.ErrNotFound) {
						slog.Warn("Could not save onboarding response milestone")
					}
				} else {
					responseDone.Store(s.accessSnapshot().Progress().ResponseVerified)
				}
			} else if subject.Kind != control.KindPerson && subject.ID != "" && !appDone.Load() {
				if err := store.MarkAppConnected(ctx); err != nil {
					slog.Warn("Could not save onboarding application milestone")
				} else {
					appDone.Store(true)
				}
			}
		})
	}
	return nil
}

// Resolve only daemon configuration, before publishing its proof. Recovery
// clients then use this literal local address without DNS or environment proxies.
func localRecoveryHost(ctx context.Context, bind string) (string, error) {
	switch bind {
	case "", "0.0.0.0", "localhost":
		return "127.0.0.1", nil
	case "::", "[::]":
		return "::1", nil
	}
	if localproof.Address(bind) {
		return bind, nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, bind)
	if err != nil {
		return "", fmt.Errorf("resolve local recovery bind address: %w", err)
	}
	for _, address := range addresses {
		if localproof.Address(address.IP.String()) {
			return address.IP.String(), nil
		}
	}
	return "", fmt.Errorf("owner recovery requires the configured bind address to resolve to this host")
}

// mutatePeople serializes membership and grants with team deletion. OAuth is
// called only after this returns, preserving the documented lock order.
func (s *Server) mutatePeople(change func() error) error {
	if s.services.Teams == nil || s.accessStore.People() == nil {
		return access.ErrClosed
	}
	s.services.Teams.mu.Lock()
	defer s.services.Teams.mu.Unlock()
	return change()
}

func (s *Server) initialOwnerTeam(ctx context.Context) (string, error) {
	// Caller holds TeamsService.mu. A published team is already durable.
	for id, team := range s.accessSnapshot().Teams() {
		if team.Kind == teams.KindShared && !team.Suspended && team.Metadata["owner_workspace"] == "true" {
			return id, nil
		}
	}
	id := "workspace-" + uuid.NewString()
	_, err := s.accessStore.CreateTeam(ctx, id, teams.Team{Name: "Personal workspace", Kind: teams.KindShared, CreatedBy: "installation", Metadata: map[string]string{"owner_workspace": "true"}}, audit.Event{})
	return id, err
}

func peopleIssuer(ref control.CredentialReference) (people.Issuer, error) {
	data, err := json.Marshal(ref)
	if err != nil {
		return people.Issuer{}, err
	}
	issuer := people.Issuer{Reference: string(data)}
	if ref.Kind == control.KindPerson {
		issuer.PersonID, issuer.Revision = ref.ID, ref.Version
	}
	return issuer, nil
}

func localRecoveryRequest(rhost string) bool {
	host, _, err := net.SplitHostPort(rhost)
	return err == nil && localproof.Address(host)
}

// accessSnapshot is the server's one read of access records.
func (s *Server) accessSnapshot() *access.Snapshot { return access.SnapshotOf(s.accessStore) }

// closeAccess releases the owner proof before the store.
func (s *Server) closeAccess() error {
	var err error
	if s.ownerRecovery != nil {
		err = s.ownerRecovery.Close()
	}
	if s.accessStore != nil {
		err = errors.Join(err, s.accessStore.Close())
	}
	return err
}

func inviteURL(issuer string, grant *people.IssuedGrant) string {
	kind := "invite"
	if grant.Purpose == people.PurposeRecovery {
		kind = "recover"
	}
	return strings.TrimSuffix(issuer, "/") + "/console/#" + kind + "?token=" + url.QueryEscape(grant.Token)
}
