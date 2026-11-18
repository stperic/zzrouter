package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/audit"
)

func (s *Server) registerPeopleRoutes() {
	if s.accessStore.People() == nil {
		return
	}
	group := s.engine.Group("/zzrouter/v1", s.browserCoordinatorOnly, browserResponseHeaders)
	group.GET("/access/setup", s.browserOriginOnly, func(c *gin.Context) {
		respondRetrievalSuccess(c, "Setup", gin.H{"initialized": s.accessStore.Snapshot().Initialized()})
	})
	group.POST("/session/password", s.handlePasswordLogin)
	public := group.Group("/access", s.requirePeopleBrowserProof)
	public.POST("/console-grants/exchange", s.handleConsoleGrantExchange)
	public.POST("/setup", s.handleOwnerClaim)
	public.POST("/invitations/inspect", s.handleInvitationInspect)
	public.POST("/invitations/accept", s.handleInvitationAccept)
	public.POST("/recovery", s.handlePeopleRecovery)
	// Local recovery has its own filesystem proof and rejects all browser input.
	group.POST("/access/recovery/local", s.handleLocalOwnerRecovery)
	self := group.Group("/access", func(c *gin.Context) {
		if s.access.authenticateMultiRole(c, []UserRole{RoleAdmin, RoleUser}, true) {
			c.Next()
		}
	})
	self.POST("/console-grants", s.handleConsoleGrantCreate)
	self.POST("/password", s.handlePasswordChange)
	self.POST("/sessions/revoke", s.handlePeopleRevokeSessions)
	admin := group.Group("/access", s.auth.browserAdminAuthMiddleware())
	admin.GET("/people", s.handlePeopleList)
	admin.GET("/people/:id", s.handlePersonGet)
	admin.PATCH("/people/:id", s.handlePersonUpdate)
	admin.POST("/people/:id/recovery", s.handleMemberRecovery)
	admin.GET("/invitations", s.handleInvitationsList)
	admin.POST("/invitations", s.handleInvitationCreate)
	admin.DELETE("/invitations/:id", s.handleInvitationRevoke)
	admin.GET("/onboarding", func(c *gin.Context) {
		respondRetrievalSuccess(c, "Getting started", s.getOnboardingProgress(c.Query("node")))
	})
	admin.PATCH("/onboarding", s.handleOnboardingUpdate)
}

func (s *Server) requirePeopleBrowserProof(c *gin.Context) {
	if err := s.access.oauth.CheckBrowserMutation(c.Request); err != nil {
		browserSessionError(c, err)
		c.Abort()
		return
	}
	c.Next()
}

// readPeopleInput bounds every pre-authentication body and refuses ambiguous JSON.
func readPeopleInput(c *gin.Context, target any) bool {
	body, err := oauth.ReadBody(c.Writer, c.Request, 8<<10)
	if err != nil {
		BadRequest(c, "Could not read the access request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		BadRequest(c, "Invalid access request fields")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		BadRequest(c, "Expected one access request")
		return false
	}
	return true
}

// loginPerson calls the sole session owner after a domain authentication or
// enrollment transaction. The callback never holds a team/people lock on return.
func (s *Server) loginPerson(c *gin.Context, authenticate func(context.Context) (*people.Person, error)) {
	var accountErr error
	info, err := s.access.oauth.LoginWith(auditContext(c), c.Writer, c.Request, func(ctx context.Context) (control.CredentialReference, error) {
		person, err := authenticate(ctx)
		accountErr = err
		if err != nil {
			return control.CredentialReference{}, err
		}
		return s.access.core.PersonReference(person)
	})
	if err != nil {
		if accountErr != nil {
			writePeopleError(c, accountErr)
		} else {
			browserSessionError(c, err)
		}
		return
	}
	respondSuccess(c, "Signed in", s.browserState(info))
}

func (s *Server) handlePasswordLogin(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	s.loginPerson(c, func(ctx context.Context) (*people.Person, error) {
		return s.accessStore.People().Authenticate(ctx, input.Username, input.Password)
	})
}

func (s *Server) handleConsoleGrantCreate(c *gin.Context) {
	var input struct{}
	if !readPeopleInput(c, &input) {
		return
	}
	ref, err := s.selfBinding(c)
	if err != nil {
		writePeopleError(c, people.ErrInvalidCredentials)
		return
	}
	issuer, err := peopleIssuer(ref)
	if err != nil {
		writePeopleError(c, err)
		return
	}
	request := people.GrantRequest{Purpose: people.PurposeSession, Issuer: issuer, TTL: 10 * time.Minute}
	var grant *people.IssuedGrant
	err = s.mutatePeople(func() error {
		if !s.accessStore.Snapshot().Initialized() && ref.Kind != control.KindPerson && !ref.IsVirtual && ref.ID == "admin" && ref.Role == control.RoleAdmin {
			teamID, err := s.initialOwnerTeam(c.Request.Context())
			if err != nil {
				return err
			}
			request.Purpose, request.TeamID, request.Role = people.PurposeSetup, teamID, "admin"
		}
		var err error
		grant, err = s.accessStore.People().Issue(auditContext(c), request)
		return err
	})
	if err != nil {
		writePeopleError(c, err)
		return
	}
	respondCreatedWithMessage(c, "Temporary console code", struct {
		*people.IssuedGrant
		URL string `json:"url"`
	}{grant, s.access.oauth.Issuer() + "/console/"})
}

func (s *Server) handleConsoleGrantExchange(c *gin.Context) {
	var input struct {
		Token string `json:"token"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	grant, err := s.accessStore.People().Inspect(c.Request.Context(), input.Token)
	if err != nil {
		writePeopleError(c, err)
		return
	}
	proof := c.GetHeader("X-CSRF-Token")
	if grant.Purpose != people.PurposeSession {
		var bound *people.IssuedGrant
		err := s.mutatePeople(func() error {
			var err error
			bound, err = s.accessStore.People().Exchange(c.Request.Context(), input.Token, proof)
			return err
		})
		if err != nil {
			writePeopleError(c, err)
			return
		}
		respondSuccess(c, "Code accepted", bound)
		return
	}
	var grantErr error
	_, err = s.access.oauth.LoginWith(auditContext(c), c.Writer, c.Request, func(ctx context.Context) (control.CredentialReference, error) {
		var issuer people.Issuer
		grantErr = s.mutatePeople(func() error {
			bound, err := s.accessStore.People().Exchange(ctx, input.Token, proof)
			if err != nil {
				return err
			}
			issuer, err = s.accessStore.People().ConsumeSession(ctx, bound.Token, proof)
			return err
		})
		if grantErr != nil {
			return control.CredentialReference{}, grantErr
		}
		var ref control.CredentialReference
		err := json.Unmarshal([]byte(issuer.Reference), &ref)
		return ref, err
	})
	if err != nil {
		if grantErr != nil {
			writePeopleError(c, grantErr)
		} else {
			browserSessionError(c, err)
		}
		return
	}
	respondSuccess(c, "Signed in", gin.H{"session": true})
}

type accountEnrollmentInput struct {
	Token string `json:"token"`
	people.CreatePerson
}

func (s *Server) handleOwnerClaim(c *gin.Context)       { s.handlePersonEnrollment(c, true) }
func (s *Server) handleInvitationAccept(c *gin.Context) { s.handlePersonEnrollment(c, false) }

func (s *Server) handlePersonEnrollment(c *gin.Context, owner bool) {
	var input accountEnrollmentInput
	if !readPeopleInput(c, &input) {
		return
	}
	s.loginPerson(c, func(ctx context.Context) (*people.Person, error) {
		var person *people.Person
		err := s.mutatePeople(func() error {
			var err error
			if owner {
				person, err = s.accessStore.People().ClaimOwner(ctx, input.Token, c.GetHeader("X-CSRF-Token"), input.CreatePerson)
			} else {
				person, err = s.accessStore.People().AcceptInvitation(ctx, input.Token, c.GetHeader("X-CSRF-Token"), input.CreatePerson)
			}
			return err
		})
		return person, err
	})
}

func (s *Server) handleInvitationInspect(c *gin.Context) {
	var input struct {
		Token string `json:"token"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	grant, err := s.accessStore.People().Inspect(c.Request.Context(), input.Token)
	if err != nil {
		writePeopleError(c, err)
		return
	}
	if grant.Purpose != people.PurposeInvitation && grant.Purpose != people.PurposeRecovery {
		writePeopleError(c, people.ErrInvalidGrant)
		return
	}
	respondRetrievalSuccess(c, "Invitation", grant)
}

func (s *Server) handlePeopleRecovery(c *gin.Context) {
	var input struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	s.loginPerson(c, func(ctx context.Context) (*people.Person, error) {
		var person *people.Person
		err := s.mutatePeople(func() error {
			var err error
			person, err = s.accessStore.People().Recover(ctx, input.Token, c.GetHeader("X-CSRF-Token"), input.Password)
			return err
		})
		return person, err
	})
}

func (s *Server) handleLocalOwnerRecovery(c *gin.Context) {
	if len(c.Request.Header.Values("Origin")) != 0 || browserCredentialPresented(c.Request) || !localRecoveryRequest(c.Request.RemoteAddr) {
		Forbidden(c, "Use the local recovery command on the installed host")
		return
	}
	var input struct {
		Proof string `json:"proof"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	if !s.ownerRecovery.Verify(input.Proof) {
		Forbidden(c, "Local installation proof is invalid")
		return
	}
	owner := s.accessStore.Snapshot().Owner()
	if owner == nil {
		Conflict(c, "Create the owner account through console setup first")
		return
	}
	var grant *people.IssuedGrant
	err := s.mutatePeople(func() error {
		var err error
		grant, err = s.accessStore.People().Issue(audit.WithIdentity(c.Request.Context(), audit.ActorLocalOperator, c.GetString("request_id")), people.GrantRequest{Purpose: people.PurposeRecovery, Issuer: people.Issuer{Reference: s.ownerRecovery.Reference()}, PersonID: owner.ID, TeamID: owner.TeamID, Role: owner.Role, TTL: 10 * time.Minute})
		return err
	})
	if err != nil {
		writePeopleError(c, err)
		return
	}
	respondSuccess(c, "Owner recovery code", struct {
		*people.IssuedGrant
		URL string `json:"url"`
	}{grant, s.access.oauth.Issuer() + "/console/"})
}

func (s *Server) handlePeopleList(c *gin.Context) {
	items := s.accessStore.Snapshot().People()
	respondList(c, items, len(items), false)
}

func (s *Server) handlePersonGet(c *gin.Context) {
	person := s.accessStore.Snapshot().Person(c.Param("id"))
	if person == nil {
		writePeopleError(c, people.ErrNotFound)
		return
	}
	respondRetrievalSuccess(c, "Person", person)
}

func (s *Server) handlePersonUpdate(c *gin.Context) {
	var input people.UpdatePerson
	if !readPeopleInput(c, &input) {
		return
	}
	var person *people.Person
	err := s.mutatePeople(func() error {
		var err error
		person, err = s.accessStore.People().Update(auditContext(c), c.Param("id"), input)
		return err
	})
	if err != nil {
		writePeopleError(c, err)
		return
	}
	respondSuccess(c, "Person updated", person)
}

func (s *Server) handleInvitationsList(c *gin.Context) {
	items := s.accessStore.Snapshot().Invitations()
	respondList(c, items, len(items), false)
}

func (s *Server) handleInvitationCreate(c *gin.Context) {
	var input struct {
		TeamID string `json:"team_id"`
		Role   string `json:"role"`
		Hours  int    `json:"expires_in_hours,omitempty"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	if input.Hours == 0 {
		input.Hours = 24
	}
	if input.Hours < 1 || input.Hours > 168 {
		BadRequest(c, "Invitation expiry must be between 1 and 168 hours")
		return
	}
	s.issueInvitation(c, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: input.TeamID, Role: input.Role, TTL: time.Duration(input.Hours) * time.Hour})
}

func (s *Server) handleMemberRecovery(c *gin.Context) {
	var input struct{}
	if !readPeopleInput(c, &input) {
		return
	}
	person := s.accessStore.Snapshot().Person(c.Param("id"))
	if person == nil {
		writePeopleError(c, people.ErrNotFound)
		return
	}
	if person.Owner {
		Forbidden(c, "Recover the owner through the local recovery command")
		return
	}
	s.issueInvitation(c, people.GrantRequest{Purpose: people.PurposeRecovery, PersonID: person.ID, TeamID: person.TeamID, Role: person.Role, TTL: 10 * time.Minute})
}

func (s *Server) issueInvitation(c *gin.Context, request people.GrantRequest) {
	ref, err := s.selfBinding(c)
	if err != nil {
		writePeopleError(c, people.ErrInvalidCredentials)
		return
	}
	request.Issuer, err = peopleIssuer(ref)
	if err != nil {
		writePeopleError(c, err)
		return
	}
	var grant *people.IssuedGrant
	err = s.mutatePeople(func() error {
		var err error
		grant, err = s.accessStore.People().Issue(auditContext(c), request)
		return err
	})
	if err != nil {
		writePeopleError(c, err)
		return
	}
	respondCreatedWithMessage(c, "Invitation created", struct {
		*people.IssuedGrant
		URL string `json:"url"`
	}{grant, inviteURL(s.access.oauth.Issuer(), grant)})
}

func (s *Server) handleInvitationRevoke(c *gin.Context) {
	if err := s.accessStore.People().RevokeGrant(auditContext(c), c.Param("id")); err != nil {
		writePeopleError(c, err)
		return
	}
	respondSuccess(c, "Invitation revoked", nil)
}

func (s *Server) currentPerson(c *gin.Context) *people.Person {
	ref, err := s.selfBinding(c)
	if err != nil || ref.Kind != control.KindPerson {
		Forbidden(c, "Sign in with a person account to manage its password and sessions")
		return nil
	}
	person := s.accessStore.Snapshot().Person(ref.ID)
	// Preserve the authenticated revision. A reset between middleware and this
	// handler must not grant an older session the authority of the new revision.
	if person == nil || person.Revision != ref.Version {
		browserSessionError(c, oauth.ErrUnauthenticated)
		return nil
	}
	return person
}

func (s *Server) handlePasswordChange(c *gin.Context) {
	var input struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if !readPeopleInput(c, &input) {
		return
	}
	person := s.currentPerson(c)
	if person == nil {
		return
	}
	s.loginPerson(c, func(ctx context.Context) (*people.Person, error) {
		return s.accessStore.People().ChangePassword(ctx, person.ID, person.Revision, input.Current, input.Password)
	})
}

func (s *Server) handlePeopleRevokeSessions(c *gin.Context) {
	var input struct{}
	if !readPeopleInput(c, &input) {
		return
	}
	person := s.currentPerson(c)
	if person == nil {
		return
	}
	s.loginPerson(c, func(ctx context.Context) (*people.Person, error) {
		return s.accessStore.People().RevokeSessions(ctx, person.ID, person.Revision)
	})
}

func (s *Server) handleOnboardingUpdate(c *gin.Context) {
	var input people.ProgressUpdate
	if !readPeopleInput(c, &input) {
		return
	}
	if err := s.accessStore.People().UpdateProgress(c.Request.Context(), input); err != nil {
		writePeopleError(c, err)
		return
	}
	respondSuccess(c, "Getting started updated", s.getOnboardingProgress(""))
}

func writePeopleError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, people.ErrInvalidCredentials):
		Unauthorized(c, "Invalid username or password")
	case errors.Is(err, people.ErrInvalidGrant):
		BadRequest(c, "This code or invitation is expired, already used, or no longer allowed. Request a new one.")
	case errors.Is(err, people.ErrInvalidInput):
		BadRequest(c, err.Error())
	case errors.Is(err, people.ErrOwnerProtected):
		Forbidden(c, "The owner account and its team are protected. Use local owner recovery if needed.")
	case errors.Is(err, people.ErrConflict), errors.Is(err, people.ErrInitialized):
		Conflict(c, "This account or setup has changed. Sign in or refresh before continuing.")
	case errors.Is(err, people.ErrNotFound):
		NotFound(c, "Person not found")
	case errors.Is(err, people.ErrLimited), errors.Is(err, people.ErrCapacity):
		RespondWithProblem(c, http.StatusTooManyRequests, "Too Many Requests", "Access requests are temporarily limited. Try again later.")
	default:
		RespondWithProblem(c, http.StatusServiceUnavailable, "Service Unavailable", "Could not save the access change. Check current state before trying again.")
	}
}
