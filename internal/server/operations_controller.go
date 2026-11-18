package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// Both public namespaces share the same handlers; their existing audience middleware stays distinct.
func (s *Server) registerOperationRoutes(router *gin.RouterGroup) {
	router.GET("/operations", s.handleListOperations)
	router.GET("/operations/:id", s.handleDescribeOperation)
	router.POST("/operations/:id/execute", s.handleExecuteOperation)
	router.GET("/connections", s.handleListConnections)
}

type operationEvidence struct {
	Target         prov_apps.OperationTarget `json:"target"`
	TargetRevision string                    `json:"target_revision"`
	State          string                    `json:"state"`
	Availability   string                    `json:"availability"`
	Source         string                    `json:"source"`
}

type operationSummary struct {
	ID                 string              `json:"id"`
	Provider           string              `json:"provider"`
	Version            int                 `json:"version"`
	DefinitionRevision string              `json:"definition_revision"`
	Description        string              `json:"description"`
	Audience           string              `json:"audience"`
	Effect             string              `json:"effect"`
	TargetScope        string              `json:"target_scope"`
	Transport          string              `json:"transport"`
	Targets            []operationEvidence `json:"targets"`
}

func (s *Server) operationVisible(c *gin.Context, operation *config.CompiledOperation) bool {
	ac := GetAccessContext(c)
	return ac != nil && ac.Key != nil && ac.Key.Role == RoleAdmin && operation.Definition().Audience == "admin"
}

func operationView(operation *config.CompiledOperation, connections []connectionInfo, c *gin.Context) operationSummary {
	d := operation.Definition()
	view := operationSummary{ID: operation.ID(), Provider: operation.Provider(), Version: d.Version, DefinitionRevision: operation.Revision(), Description: d.Description, Audience: d.Audience, Effect: d.Effect, TargetScope: d.TargetScope, Transport: d.Transport, Targets: []operationEvidence{}}
	for _, connection := range connections {
		if connection.RunID == "" || connection.Provider != operation.Provider() || !operation.AppliesToRuntime(connection.Runtime) || !connection.matches(c) {
			continue
		}
		view.Targets = append(view.Targets, operationEvidence{Target: connection.target(), TargetRevision: connection.TargetRevision, State: connection.State, Availability: "unknown", Source: "definition_and_run_snapshot"})
	}
	return view
}

func (s *Server) handleListOperations(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	offset, limit, ok := providerDiscoveryPage(c)
	if !ok {
		return
	}
	visible := []*config.CompiledOperation{}
	s.appsConfig().RangeOperations(func(operation *config.CompiledOperation) bool {
		if s.operationVisible(c, operation) && (c.Query("provider") == "" || c.Query("provider") == operation.Provider()) {
			visible = append(visible, operation)
		}
		return true
	})
	views := []operationSummary{}
	if len(visible) > 0 {
		connections, err := s.readConnections(c)
		if err != nil {
			RespondToError(c, err)
			return
		}
		for _, operation := range visible {
			views = append(views, operationView(operation, connections, c))
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	total := len(views)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	respondList(c, views[offset:end], total, end < total)
}

func providerDiscoveryPage(c *gin.Context) (int, int, bool) {
	offset, limit := 0, 50
	for name, destination := range map[string]*int{"offset": &offset, "limit": &limit} {
		if raw := c.Query(name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || name == "limit" && (value < 1 || value > 100) {
				BadRequest(c, "offset must be nonnegative and limit must be between 1 and 100")
				return 0, 0, false
			}
			*destination = value
		}
	}
	return offset, limit, true
}

func (s *Server) visibleOperation(c *gin.Context) (*config.CompiledOperation, bool) {
	operation, found := s.appsConfig().Operation(c.Param("id"))
	if !found || !s.operationVisible(c, operation) {
		RespondWithProblemOpts(c, http.StatusNotFound, "Not Found", "Operation is not visible to this caller", ProblemOpts{Code: "operation_not_found"})
		return nil, false
	}
	return operation, true
}

func (s *Server) handleDescribeOperation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	operation, ok := s.visibleOperation(c)
	if !ok {
		return
	}
	connections, err := s.readConnections(c)
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Operation definition", struct {
		operationSummary
		Definition config.OperationDefinition `json:"definition"`
	}{operationView(operation, connections, c), operation.Definition()})
}

func bindOperation(c *gin.Context) (prov_apps.OperationRequest, bool) {
	var request prov_apps.OperationRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	var payload json.RawMessage
	err := decoder.Decode(&payload)
	if err == nil && !errors.Is(decoder.Decode(new(any)), io.EOF) {
		err = errors.New("trailing JSON")
	}
	if err == nil {
		decoder = json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		err = decoder.Decode(&request)
	}
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			RespondWithProblem(c, http.StatusRequestEntityTooLarge, "Request Entity Too Large", "Operation request exceeds 1 MiB")
		} else {
			BadRequest(c, "Send one operation request object containing only declared fields")
		}
		return request, false
	}
	var fields map[string]json.RawMessage
	var targetFields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err == nil {
		_ = json.Unmarshal(fields["target"], &targetFields)
	}
	_, inputPresent := fields["input"]
	if !inputPresent || len(targetFields["model"]) == 0 || len(targetFields["runtime"]) == 0 || bytes.Equal(bytes.TrimSpace(targetFields["model"]), []byte("null")) || bytes.Equal(bytes.TrimSpace(targetFields["runtime"]), []byte("null")) {
		BadRequest(c, "input and the copied target.model and target.runtime fields are required; explicit null input is allowed only by the selected schema")
		return request, false
	}
	if request.Version < 1 || request.DefinitionRevision == "" || request.Preconditions.TargetRevision == "" || request.Target.Node == "" || request.Target.Provider == "" || request.Target.RunID == "" || strings.ContainsAny(request.Target.RunID, "/\\\x00\r\n") {
		BadRequest(c, "version, definition_revision, preconditions.target_revision, target.provider, target.node and target.run_id are required")
		return request, false
	}
	return request, true
}

func (s *Server) handleExecuteOperation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	operation, ok := s.visibleOperation(c)
	if !ok {
		return
	}
	request, ok := bindOperation(c)
	if !ok {
		return
	}
	if request.Target.Provider != operation.Provider() || !s.knownClusterNodes()[request.Target.Node] {
		RespondWithProblemOpts(c, http.StatusConflict, "Conflict", "Target must name the operation's provider and an exact known node name", ProblemOpts{Code: "operation_target_mismatch"})
		return
	}
	if request.Version != operation.Definition().Version || request.DefinitionRevision != operation.Revision() {
		RespondWithProblemOpts(c, http.StatusConflict, "Conflict", "Discover the current operation definition before executing", ProblemOpts{Code: "operation_definition_stale"})
		return
	}
	if operation.ValidateInput(request.Input) != nil {
		RespondWithProblemOpts(c, http.StatusBadRequest, "Bad Request", "Input does not satisfy the operation schema", ProblemOpts{Code: "operation_input_invalid"})
		return
	}
	body, err := json.Marshal(request)
	if err != nil {
		BadRequest(c, "Invalid operation input")
		return
	}
	// Unicast uses the existing local-dispatch or cluster mTLS transport, never a worker admin port.
	response, err := s.cluster.router.Unicast(c.Request.Context(), request.Target.Node, "/zzrouter/v1/internal/operations/"+url.PathEscape(operation.ID())+"/execute", http.MethodPost, body)
	if err != nil {
		RespondToError(c, newRoutedTransportError(err, request.Target.Node))
		return
	}
	if response.StatusCode >= 400 {
		RespondToError(c, parseRoutedError(response.StatusCode, response.Body, response.Headers))
		return
	}
	c.Data(response.StatusCode, "application/json", response.Body)
}

func (s *Server) handleInternalExecuteOperation(c *gin.Context) {
	request, ok := bindOperation(c)
	if !ok {
		return
	}
	if s.providers.appMgr == nil {
		ServiceUnavailable(c, "Provider manager unavailable")
		return
	}
	result, err := s.providers.appMgr.ExecuteOperation(c.Request.Context(), c.Param("id"), request, s.httpClient)
	if err != nil {
		var refused *prov_apps.OperationError
		if errors.As(err, &refused) {
			RespondWithProblemOpts(c, refused.Status, http.StatusText(refused.Status), refused.Detail, ProblemOpts{Code: refused.Code, Node: s.node.Name()})
		} else {
			RespondToError(c, err)
		}
		return
	}
	respondSuccess(c, "Operation completed", result)
}
