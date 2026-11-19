package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/people"
)

// validateExternalIssuer runs inside a people commit, under the access store's writer gate, so it never reads a person.
func (s *Server) validateExternalIssuer(_ context.Context, value string) error {
	if s.localProof.ValidReference(value) {
		return nil
	}
	var ref control.CredentialReference
	if err := json.Unmarshal([]byte(value), &ref); err != nil || ref.Kind == control.KindPerson {
		return people.ErrInvalidGrant
	}
	_, err := s.access.core.AuthenticateReference(ref)
	return err
}

// initPeople records onboarding milestones from served requests.
func (s *Server) initPeople(ctx context.Context) {
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

// accessSnapshot is the server's one read of access records.
func (s *Server) accessSnapshot() *access.Snapshot { return access.SnapshotOf(s.accessStore) }

// closeAccess releases the local proof before the store.
func (s *Server) closeAccess() error {
	err := s.localProof.Close()
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
