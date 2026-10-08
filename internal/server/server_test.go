package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
)

// nilAppsConfig is a test helper that returns a nil-returning appsConfig accessor.
var nilAppsConfig = func() *pkgConfig.AppsConfig { return nil }

func init() {
	gin.SetMode(gin.TestMode)
}

// ============================================================================
// Validation Tests
// ============================================================================

func TestValidationError_Error(t *testing.T) {
	err := &ValidationError{
		Parameter: "model_name",
		Message:   "is required",
	}

	expected := "parameter 'model_name': is required"
	if got := err.Error(); got != expected {
		t.Errorf("Error() = %q, want %q", got, expected)
	}
}

func TestValidationResult_Getters(t *testing.T) {
	result := &ValidationResult{
		strings: map[string]string{"name": "test-value"},
		ints:    map[string]int{"limit": 100},
		floats:  make(map[string]float64),
		bools:   map[string]bool{"verbose": true},
	}

	if got := result.GetString("name"); got != "test-value" {
		t.Errorf("GetString = %q", got)
	}
	if got := result.GetString("nonexistent"); got != "" {
		t.Errorf("GetString missing = %q", got)
	}
	if got := result.GetInt("limit"); got != 100 {
		t.Errorf("GetInt = %d", got)
	}
	if got := result.GetInt("nonexistent"); got != 0 {
		t.Errorf("GetInt missing = %d", got)
	}
	if got := result.GetBool("verbose"); !got {
		t.Error("GetBool = false")
	}
	if got := result.GetBool("nonexistent"); got {
		t.Error("GetBool missing = true")
	}
}

func TestParamRuleBuilders(t *testing.T) {
	t.Run("RequiredPathParam", func(t *testing.T) {
		rule := RequiredPathParam("model_name")
		if rule.Name != "model_name" {
			t.Errorf("Name = %q, want %q", rule.Name, "model_name")
		}
		if rule.Source != ParamSourcePath {
			t.Errorf("Source = %v, want ParamSourcePath", rule.Source)
		}
		if !rule.Required {
			t.Error("Required should be true")
		}
	})

	t.Run("OptionalQueryParam", func(t *testing.T) {
		rule := OptionalQueryParam("filter")
		if rule.Name != "filter" {
			t.Errorf("Name = %q, want %q", rule.Name, "filter")
		}
		if rule.Source != ParamSourceQuery {
			t.Errorf("Source = %v, want ParamSourceQuery", rule.Source)
		}
		if rule.Required {
			t.Error("Required should be false")
		}
	})

	t.Run("OptionalQueryParamWithDefault", func(t *testing.T) {
		rule := OptionalQueryParamWithDefault("sort", "asc")
		if rule.Default != "asc" {
			t.Errorf("Default = %q, want %q", rule.Default, "asc")
		}
	})

	t.Run("RequiredQueryParam", func(t *testing.T) {
		rule := RequiredQueryParam("id")
		if !rule.Required {
			t.Error("Required should be true")
		}
	})

	t.Run("BoolQueryParam", func(t *testing.T) {
		rule := BoolQueryParam("verbose")
		if rule.Type != ParamTypeBool {
			t.Errorf("Type = %v, want ParamTypeBool", rule.Type)
		}
		if rule.Default != "false" {
			t.Errorf("Default = %q, want %q", rule.Default, "false")
		}
	})

	t.Run("IntQueryParam", func(t *testing.T) {
		rule := IntQueryParam("limit", 20, 1, 100)
		if rule.Type != ParamTypeInt {
			t.Errorf("Type = %v, want ParamTypeInt", rule.Type)
		}
		if rule.Default != "20" {
			t.Errorf("Default = %q, want %q", rule.Default, "20")
		}
		if *rule.MinValue != 1 {
			t.Errorf("MinValue = %d, want 1", *rule.MinValue)
		}
		if *rule.MaxValue != 100 {
			t.Errorf("MaxValue = %d, want 100", *rule.MaxValue)
		}
	})

}

// ============================================================================
// Validation Type Tests
// ============================================================================

func TestValidateString(t *testing.T) {
	tests := []struct {
		name      string
		rule      ParamRule
		value     string
		wantErr   bool
		wantValue string
	}{
		{
			name:      "valid string",
			rule:      ParamRule{Name: "test"},
			value:     "hello",
			wantErr:   false,
			wantValue: "hello",
		},
		{
			name:    "too short",
			rule:    ParamRule{Name: "test", MinLength: 5},
			value:   "hi",
			wantErr: true,
		},
		{
			name:    "too long",
			rule:    ParamRule{Name: "test", MaxLength: 5},
			value:   "hello world",
			wantErr: true,
		},
		{
			name:    "not in allowed values",
			rule:    ParamRule{Name: "test", AllowedValues: []string{"a", "b", "c"}},
			value:   "d",
			wantErr: true,
		},
		{
			name:      "in allowed values",
			rule:      ParamRule{Name: "test", AllowedValues: []string{"a", "b", "c"}},
			value:     "b",
			wantErr:   false,
			wantValue: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &ValidationResult{strings: make(map[string]string)}
			err := validateString(tt.rule, tt.value, result)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateString() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && result.strings[tt.rule.Name] != tt.wantValue {
				t.Errorf("result value = %q, want %q", result.strings[tt.rule.Name], tt.wantValue)
			}
		})
	}
}

func TestValidateInt(t *testing.T) {
	minVal := 1
	maxVal := 100

	tests := []struct {
		name      string
		rule      ParamRule
		value     string
		wantErr   bool
		wantValue int
	}{
		{
			name:      "valid int",
			rule:      ParamRule{Name: "test"},
			value:     "42",
			wantErr:   false,
			wantValue: 42,
		},
		{
			name:    "invalid int",
			rule:    ParamRule{Name: "test"},
			value:   "not-a-number",
			wantErr: true,
		},
		{
			name:    "below min",
			rule:    ParamRule{Name: "test", MinValue: &minVal},
			value:   "0",
			wantErr: true,
		},
		{
			name:    "above max",
			rule:    ParamRule{Name: "test", MaxValue: &maxVal},
			value:   "101",
			wantErr: true,
		},
		{
			name:      "within range",
			rule:      ParamRule{Name: "test", MinValue: &minVal, MaxValue: &maxVal},
			value:     "50",
			wantErr:   false,
			wantValue: 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &ValidationResult{ints: make(map[string]int)}
			err := validateInt(tt.rule, tt.value, result)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateInt() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && result.ints[tt.rule.Name] != tt.wantValue {
				t.Errorf("result value = %d, want %d", result.ints[tt.rule.Name], tt.wantValue)
			}
		})
	}
}

func TestValidateFloat(t *testing.T) {
	tests := []struct {
		name      string
		rule      ParamRule
		value     string
		wantErr   bool
		wantValue float64
	}{
		{
			name:      "valid float",
			rule:      ParamRule{Name: "test"},
			value:     "3.14",
			wantErr:   false,
			wantValue: 3.14,
		},
		{
			name:    "invalid float",
			rule:    ParamRule{Name: "test"},
			value:   "not-a-number",
			wantErr: true,
		},
		{
			name:      "integer as float",
			rule:      ParamRule{Name: "test"},
			value:     "42",
			wantErr:   false,
			wantValue: 42.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &ValidationResult{floats: make(map[string]float64)}
			err := validateFloat(tt.rule, tt.value, result)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFloat() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && result.floats[tt.rule.Name] != tt.wantValue {
				t.Errorf("result value = %f, want %f", result.floats[tt.rule.Name], tt.wantValue)
			}
		})
	}
}

func TestValidateBool(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantErr   bool
		wantValue bool
	}{
		{"true", "true", false, true},
		{"false", "false", false, false},
		{"1", "1", false, true},
		{"0", "0", false, false},
		{"yes", "yes", false, true},
		{"no", "no", false, false},
		{"TRUE (case insensitive)", "TRUE", false, true},
		{"FALSE (case insensitive)", "FALSE", false, false},
		{"empty string", "", false, false},
		{"invalid", "maybe", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := ParamRule{Name: "test"}
			result := &ValidationResult{bools: make(map[string]bool)}
			err := validateBool(rule, tt.value, result)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBool() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && result.bools["test"] != tt.wantValue {
				t.Errorf("result value = %v, want %v", result.bools["test"], tt.wantValue)
			}
		})
	}
}

// ============================================================================
// Response Helpers Tests
// ============================================================================

func TestParseInt(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"42", 42, false},
		{"0", 0, false},
		{"-10", -10, false},
		{"invalid", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseInt(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseInt() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseInt() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParsePagination(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{
			name:       "default values",
			query:      "",
			wantLimit:  DefaultPageLimit,
			wantOffset: 0,
		},
		{
			name:       "custom limit",
			query:      "limit=50",
			wantLimit:  50,
			wantOffset: 0,
		},
		{
			name:       "custom offset",
			query:      "offset=10",
			wantLimit:  DefaultPageLimit,
			wantOffset: 10,
		},
		{
			name:       "both custom",
			query:      "limit=25&offset=50",
			wantLimit:  25,
			wantOffset: 50,
		},
		{
			name:       "limit exceeds max",
			query:      "limit=5000",
			wantLimit:  MaxPageLimit,
			wantOffset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest("GET", "/?"+tt.query, nil)

			params, ok := ParsePagination(c)
			if !ok {
				t.Fatalf("ParsePagination rejected %q", tt.query)
			}

			if params.Limit != tt.wantLimit {
				t.Errorf("Limit = %d, want %d", params.Limit, tt.wantLimit)
			}
			if params.Offset != tt.wantOffset {
				t.Errorf("Offset = %d, want %d", params.Offset, tt.wantOffset)
			}
		})
	}
}

// A present-but-unusable parameter is a 400: substituting the default
// hands back a page the caller did not ask for and cannot distinguish
// from the one it did.
func TestParsePagination_RejectsUnusableValues(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"non-numeric limit", "limit=invalid", "limit must be a positive integer"},
		{"negative limit", "limit=-10", "limit must be a positive integer"},
		{"zero limit", "limit=0", "limit must be a positive integer"},
		{"trailing junk", "limit=10abc", "limit must be a positive integer"},
		{"negative offset", "offset=-1", "offset must be a non-negative integer"},
		{"non-numeric offset", "offset=x", "offset must be a non-negative integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest("GET", "/?"+tt.query, nil)

			_, ok := ParsePagination(c)
			if ok {
				t.Fatalf("ParsePagination accepted %q", tt.query)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), tt.want) {
				t.Errorf("body %q does not name the problem (%q)", w.Body.String(), tt.want)
			}
		})
	}
}

func TestApplyPagination(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	tests := []struct {
		name        string
		params      PaginationParams
		wantLen     int
		wantTotal   int
		wantHasMore bool
		wantFirst   string
	}{
		{
			name:        "first page",
			params:      PaginationParams{Limit: 3, Offset: 0},
			wantLen:     3,
			wantTotal:   10,
			wantHasMore: true,
			wantFirst:   "a",
		},
		{
			name:        "second page",
			params:      PaginationParams{Limit: 3, Offset: 3},
			wantLen:     3,
			wantTotal:   10,
			wantHasMore: true,
			wantFirst:   "d",
		},
		{
			name:        "last page",
			params:      PaginationParams{Limit: 3, Offset: 9},
			wantLen:     1,
			wantTotal:   10,
			wantHasMore: false,
			wantFirst:   "j",
		},
		{
			name:        "offset beyond data",
			params:      PaginationParams{Limit: 3, Offset: 20},
			wantLen:     0,
			wantTotal:   10,
			wantHasMore: false,
		},
		{
			name:        "get all",
			params:      PaginationParams{Limit: 100, Offset: 0},
			wantLen:     10,
			wantTotal:   10,
			wantHasMore: false,
			wantFirst:   "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, total, hasMore := ApplyPagination(items, tt.params)

			if len(result) != tt.wantLen {
				t.Errorf("len(result) = %d, want %d", len(result), tt.wantLen)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %d, want %d", total, tt.wantTotal)
			}
			if hasMore != tt.wantHasMore {
				t.Errorf("hasMore = %v, want %v", hasMore, tt.wantHasMore)
			}
			if tt.wantLen > 0 && result[0] != tt.wantFirst {
				t.Errorf("first element = %q, want %q", result[0], tt.wantFirst)
			}
		})
	}
}

func TestQueryHelpers(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/?registry=ollama&provider=vllm&model=llama2&node=worker-1", nil)

	if got := QueryRegistry(c); got != "ollama" {
		t.Errorf("QueryRegistry() = %q, want %q", got, "ollama")
	}
	if got := QueryProvider(c); got != "vllm" {
		t.Errorf("QueryProvider() = %q, want %q", got, "vllm")
	}
	if got := QueryModel(c); got != "llama2" {
		t.Errorf("QueryModel() = %q, want %q", got, "llama2")
	}
	if got := QueryNode(c); got != "worker-1" {
		t.Errorf("QueryNode() = %q, want %q", got, "worker-1")
	}
}

func TestRespondList(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	data := []string{"item1", "item2"}
	respondList(c, data, 10, true)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"total":10`) {
		t.Error("response should contain total")
	}
	if !strings.Contains(body, `"has_more":true`) {
		t.Error("response should contain has_more")
	}
}

func TestRespondNoContent(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	respondNoContent(c)

	// Gin's Status() sets internal state; check Writer.Status()
	// The httptest.Recorder's code is set when headers are flushed
	// which gin does lazily. We check gin's internal status instead.
	if status := c.Writer.Status(); status != http.StatusNoContent {
		t.Errorf("status = %d, want %d", status, http.StatusNoContent)
	}
}

// ============================================================================
// Problem Details Tests
// ============================================================================

func TestRespondWithProblem(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/test/path", nil)

	RespondWithProblem(c, http.StatusBadRequest, "Bad Request", "Invalid parameter")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "application/problem+json") {
		t.Errorf("Content-Type = %q, should contain application/problem+json", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"title":"Bad Request"`) {
		t.Error("response should contain title")
	}
	if !strings.Contains(body, `"detail":"Invalid parameter"`) {
		t.Error("response should contain detail")
	}
}

func TestProblemDetailHelpers(t *testing.T) {
	tests := []struct {
		name       string
		fn         func(*gin.Context, string)
		wantStatus int
	}{
		{"BadRequest", BadRequest, http.StatusBadRequest},
		{"Unauthorized", Unauthorized, http.StatusUnauthorized},
		{"Forbidden", Forbidden, http.StatusForbidden},
		{"NotFound", NotFound, http.StatusNotFound},
		{"Conflict", Conflict, http.StatusConflict},
		{"InternalNodeError", InternalNodeError, http.StatusInternalServerError},
		{"ServiceUnavailable", ServiceUnavailable, http.StatusServiceUnavailable},
		{"RequestTimeout", RequestTimeout, http.StatusRequestTimeout},
		{"BadGateway", BadGateway, http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest("GET", "/test", nil)

			tt.fn(c, "test detail")

			if w.Code != tt.wantStatus {
				t.Errorf("%s() status = %d, want %d", tt.name, w.Code, tt.wantStatus)
			}
		})
	}
}

// ============================================================================
// Types Tests
// ============================================================================

func TestErrModelNotFound(t *testing.T) {
	err := cache.ErrModelNotFound{ModelName: "test-model"}
	expected := "model 'test-model' not found"
	if got := err.Error(); got != expected {
		t.Errorf("Error() = %q, want %q", got, expected)
	}
}

func TestErrMultipleMatches(t *testing.T) {
	err := ErrMultipleMatches{
		ModelName: "llama",
		Matches:   []string{"llama2", "llama3"},
	}
	got := err.Error()
	if !strings.Contains(got, "llama") {
		t.Errorf("Error() should contain model name, got %q", got)
	}
	if !strings.Contains(got, "llama2") {
		t.Errorf("Error() should contain matches, got %q", got)
	}
}

// ============================================================================
// Helpers Tests
// ============================================================================

func TestFindMatchingModels(t *testing.T) {
	models := []*metadata.ModelMetadata{
		{Name: "llama2-7b"},
		{Name: "llama2-13b"},
		{Name: "llama3-8b"},
		{Name: "mistral-7b"},
	}

	tests := []struct {
		pattern   string
		wantCount int
	}{
		{"", 4},            // Empty pattern returns all
		{"llama2", 2},      // Matches llama2-*
		{"llama", 3},       // Matches llama*
		{"mistral", 1},     // Exact contains match
		{"nonexistent", 0}, // No matches
		{"7b", 2},          // Matches *-7b
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			matches := findMatchingModels(tt.pattern, models)
			if len(matches) != tt.wantCount {
				t.Errorf("findMatchingModels(%q) returned %d matches, want %d", tt.pattern, len(matches), tt.wantCount)
			}
		})
	}
}

func TestParseModelIdentifier(t *testing.T) {
	tests := []struct {
		input     string
		wantModel string
		wantErr   bool
	}{
		{"llama2", "llama2", false},
		{"llama2:7b", "llama2", false},
		{"ollama/llama2", "llama2", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			id, err := parseModelIdentifier(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseModelIdentifier() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && id.ModelName != tt.wantModel {
				t.Errorf("ModelName = %q, want %q", id.ModelName, tt.wantModel)
			}
		})
	}
}

// ============================================================================
// Streaming Tests
// ============================================================================

type mockFlusher struct {
	*httptest.ResponseRecorder
	flushed int
}

func (m *mockFlusher) Flush() {
	m.flushed++
}

func TestStreamNDJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	flusher := &mockFlusher{ResponseRecorder: recorder}

	// Simulate NDJSON input
	input := `{"status":"pulling"}
{"status":"verifying"}
{"status":"done"}`

	err := streamNDJSON(flusher, flusher, strings.NewReader(input))
	if err != nil {
		t.Errorf("streamNDJSON() error = %v", err)
	}

	// Should have flushed after each line
	if flusher.flushed != 3 {
		t.Errorf("flushed = %d, want 3", flusher.flushed)
	}

	// Output should contain all lines
	output := recorder.Body.String()
	if !strings.Contains(output, "pulling") {
		t.Error("output should contain 'pulling'")
	}
	if !strings.Contains(output, "done") {
		t.Error("output should contain 'done'")
	}
}

func TestStreamResponseWithFlush_NonFlushable(t *testing.T) {
	// Test with a ResponseRecorder which does implement Flusher
	input := strings.NewReader("test data")
	w := httptest.NewRecorder()
	err := streamResponseWithFlush(w, input)
	if err != nil {
		t.Errorf("streamResponseWithFlush() error = %v", err)
	}
}

// ============================================================================
// Mock Router for Service Tests
// ============================================================================

type mockRouter struct {
	response *routing.Response
	err      error
}

func (m *mockRouter) Unicast(ctx context.Context, host, path, method string, body []byte) (*routing.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockRouter) Broadcast(ctx context.Context, path, method string, body []byte) (*routing.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockRouter) MultiRoute(ctx context.Context, path string, method string, hostBodies map[string][]byte) (*routing.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

type mockNodeInfo struct {
	name     string
	isWorker bool
}

func (m *mockNodeInfo) GetNodename() string {
	return m.name
}

func (m *mockNodeInfo) GetLocalStats() map[string]any {
	return map[string]any{"cpu": 50, "memory": 75}
}

func (m *mockNodeInfo) IsWorker() bool {
	return m.isWorker
}

// ============================================================================
// AppsService Tests
// ============================================================================

// mockClusterState implements ClusterState for tests
type mockClusterState struct {
	localApps []prov_apps.LocalProviderInfo
	endpoints []*mesh.Endpoint
	localNode map[string]any
}

func (m *mockClusterState) GetLocalAppsInfo() []prov_apps.LocalProviderInfo { return m.localApps }
func (m *mockClusterState) GetLocalNodeInfo() map[string]any                { return m.localNode }
func (m *mockClusterState) GetClusterEndpoints() []*mesh.Endpoint           { return m.endpoints }
func (m *mockClusterState) IsCoordinator() bool                             { return true }
func (m *mockClusterState) RefreshClusterEndpoints()                        {}
func (m *mockClusterState) RefreshClusterEndpoint(_ string)                 {}
func (m *mockClusterState) GetStartTime() time.Time                         { return time.Now() }

func TestAppsService_ListApps(t *testing.T) {
	t.Run("success with local apps", func(t *testing.T) {
		provider := &mockClusterState{
			localApps: []prov_apps.LocalProviderInfo{
				{Key: "ollama", Name: "Ollama", Type: "ollama", Node: "node1", State: prov_apps.StateLive},
			},
		}
		svc := NewAppsService(provider, &mockRouter{})

		resp, err := svc.ListApps(context.Background(), &ListAppsRequest{})
		if err != nil {
			t.Fatalf("ListApps() error = %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("Total = %d, want 1", resp.Total)
		}
		if len(resp.Data) != 1 {
			t.Errorf("Data length = %d, want 1", len(resp.Data))
		}
	})

	t.Run("kind sourced from info.Kind, not info.Mode", func(t *testing.T) {
		// Pin the convergence: Kind on the response must come from
		// LocalProviderInfo.Kind (which p.Kind() populates with the
		// canonical kind value matching providers/<kind>/) — not from
		// LocalProviderInfo.Mode (which retains the older "service" /
		// "external" split). The catalog endpoint sources Kind from
		// p.Kind() too; same-binary-two-answers must not return.
		provider := &mockClusterState{
			localApps: []prov_apps.LocalProviderInfo{
				{
					Key: "ollama", Name: "Ollama", Type: "ollama", Node: "node1",
					State: prov_apps.StateLive,
					Kind:  "external",
					Mode:  "service",
				},
			},
		}
		svc := NewAppsService(provider, &mockRouter{})
		resp, err := svc.ListApps(context.Background(), &ListAppsRequest{})
		if err != nil {
			t.Fatalf("ListApps() error = %v", err)
		}
		if len(resp.Data) != 1 {
			t.Fatalf("Data length = %d, want 1", len(resp.Data))
		}
		if resp.Data[0].Kind != "external" {
			t.Errorf("Kind = %q, want %q (must come from info.Kind, not info.Mode)", resp.Data[0].Kind, "external")
		}
		if resp.Data[0].Mode != "service" {
			t.Errorf("Mode = %q, want %q (legacy field, preserved)", resp.Data[0].Mode, "service")
		}
	})

	t.Run("nil provider returns empty", func(t *testing.T) {
		svc := NewAppsService(nil, &mockRouter{})

		resp, err := svc.ListApps(context.Background(), &ListAppsRequest{})
		if err != nil {
			t.Fatalf("ListApps() error = %v", err)
		}
		if resp.Total != 0 {
			t.Errorf("Total = %d, want 0", resp.Total)
		}
	})

	t.Run("with filters", func(t *testing.T) {
		provider := &mockClusterState{
			localApps: []prov_apps.LocalProviderInfo{
				{Key: "ollama", Name: "Ollama", Type: "ollama", Node: "node1", State: prov_apps.StateLive},
				{Key: "vllm", Name: "vLLM", Type: "vllm", Node: "node1", State: prov_apps.StateLive},
			},
		}
		svc := NewAppsService(provider, &mockRouter{})

		req := &ListAppsRequest{App: "ollama"}
		resp, err := svc.ListApps(context.Background(), req)
		if err != nil {
			t.Fatalf("ListApps() error = %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("Total = %d, want 1", resp.Total)
		}
	})
}

func TestAppsService_GetApp(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"name":"Ollama","type":"ollama"}`),
			},
		}
		svc := NewAppsService(nil, router)

		resp, err := svc.GetApp(context.Background(), &GetAppRequest{Name: "ollama"})
		if err != nil {
			t.Fatalf("GetApp() error = %v", err)
		}
		if resp.App.Name != "Ollama" {
			t.Errorf("Name = %q, want 'Ollama'", resp.App.Name)
		}
	})

	t.Run("router error", func(t *testing.T) {
		router := &mockRouter{
			err: fmt.Errorf("not found"),
		}
		svc := NewAppsService(nil, router)

		_, err := svc.GetApp(context.Background(), &GetAppRequest{Name: "nonexistent"})
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestAppsService_UpdateApp(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"name":"ollama","enabled":true,"message":"updated"}`),
			},
		}
		svc := NewAppsService(nil, router)

		enabled := true
		resp, err := svc.UpdateApp(context.Background(), &UpdateAppRequest{
			Name:    "ollama",
			Enabled: &enabled,
		})
		if err != nil {
			t.Fatalf("UpdateApp() error = %v", err)
		}
		if !resp.Enabled {
			t.Error("Enabled should be true")
		}
	})

	t.Run("router error", func(t *testing.T) {
		router := &mockRouter{
			err: fmt.Errorf("update failed"),
		}
		svc := NewAppsService(nil, router)

		enabled := false
		_, err := svc.UpdateApp(context.Background(), &UpdateAppRequest{
			Name:    "ollama",
			Enabled: &enabled,
		})
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

// ============================================================================
// RunsService Tests
// ============================================================================

func TestRunsService_ListRuns(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"instances":[{"id":"run-1","status":"running"}]}`),
			},
		}
		svc := NewRunsService(router)

		resp, err := svc.ListRuns(context.Background(), &ListRunsRequest{})
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("Total = %d, want 1", resp.Total)
		}
	})

	t.Run("with status filter", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"instances":[{"id":"run-1","status":"running"},{"id":"run-2","status":"stopped"}]}`),
			},
		}
		svc := NewRunsService(router)

		resp, err := svc.ListRuns(context.Background(), &ListRunsRequest{Status: "running"})
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("Total = %d, want 1 (filtered)", resp.Total)
		}
	})

	t.Run("router error", func(t *testing.T) {
		router := &mockRouter{
			err: fmt.Errorf("connection failed"),
		}
		svc := NewRunsService(router)

		_, err := svc.ListRuns(context.Background(), &ListRunsRequest{})
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestRunsService_GetRun(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"id":"run-123","status":"running","node":"node1"}`),
			},
		}
		svc := NewRunsService(router)

		run, err := svc.GetRun(context.Background(), &GetRunRequest{RunID: "run-123"})
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.ID != "run-123" {
			t.Errorf("ID = %q, want 'run-123'", run.ID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		router := &mockRouter{
			err: fmt.Errorf("run not found"),
		}
		svc := NewRunsService(router)

		_, err := svc.GetRun(context.Background(), &GetRunRequest{RunID: "nonexistent"})
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestRunsService_StopRun(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"id":"run-123","status":"stopped","message":"Run stopped successfully"}`),
			},
		}
		svc := NewRunsService(router)

		resp, err := svc.StopRun(context.Background(), &StopRunRequest{RunID: "run-123"})
		if err != nil {
			t.Fatalf("StopRun() error = %v", err)
		}
		if resp.Status != "stopped" {
			t.Errorf("Status = %q, want 'stopped'", resp.Status)
		}
	})

	t.Run("router error", func(t *testing.T) {
		router := &mockRouter{
			err: fmt.Errorf("stop failed"),
		}
		svc := NewRunsService(router)

		_, err := svc.StopRun(context.Background(), &StopRunRequest{RunID: "run-123"})
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestRunsService_RestartRun(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"id":"run-123","status":"running","message":"Run restarted"}`),
			},
		}
		svc := NewRunsService(router)

		resp, err := svc.RestartRun(context.Background(), &RestartRunRequest{RunID: "run-123"})
		if err != nil {
			t.Fatalf("RestartRun() error = %v", err)
		}
		if resp.Status != "running" {
			t.Errorf("Status = %q, want 'running'", resp.Status)
		}
	})
}

// ============================================================================
// ValidateParams Integration Tests
// ============================================================================

func TestValidateParams_Integration(t *testing.T) {
	t.Run("all valid params", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest("GET", "/test?limit=50&verbose=true&name=test", nil)
		c.Params = []gin.Param{{Key: "model", Value: "llama2"}}

		rules := []ParamRule{
			RequiredPathParam("model"),
			IntQueryParam("limit", 100, 1, 1000),
			BoolQueryParam("verbose"),
			OptionalQueryParam("name"),
		}

		result := ValidateParams(c, rules)
		if result == nil {
			t.Fatal("ValidateParams() returned nil")
		}

		if model := result.GetString("model"); model != "llama2" {
			t.Errorf("model = %q, want 'llama2'", model)
		}
		if limit := result.GetInt("limit"); limit != 50 {
			t.Errorf("limit = %d, want 50", limit)
		}
		if verbose := result.GetBool("verbose"); !verbose {
			t.Error("verbose should be true")
		}
		if name := result.GetString("name"); name != "test" {
			t.Errorf("name = %q, want 'test'", name)
		}
	})

	t.Run("missing required param", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest("GET", "/test", nil)
		c.Params = []gin.Param{} // No path params

		rules := []ParamRule{
			RequiredPathParam("model"),
		}

		result := ValidateParams(c, rules)
		if result != nil {
			t.Error("ValidateParams() should return nil on validation failure")
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid int param", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest("GET", "/test?limit=notanumber", nil)

		rules := []ParamRule{
			IntQueryParam("limit", 100, 1, 1000),
		}

		result := ValidateParams(c, rules)
		if result != nil {
			t.Error("ValidateParams() should return nil for invalid int")
		}
	})

	t.Run("int out of range", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest("GET", "/test?limit=5000", nil)

		rules := []ParamRule{
			IntQueryParam("limit", 100, 1, 1000),
		}

		result := ValidateParams(c, rules)
		if result != nil {
			t.Error("ValidateParams() should return nil for out-of-range int")
		}
	})

	t.Run("default values applied", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest("GET", "/test", nil)

		rules := []ParamRule{
			IntQueryParam("limit", 100, 1, 1000),
			BoolQueryParam("verbose"),
			OptionalQueryParamWithDefault("format", "json"),
		}

		result := ValidateParams(c, rules)
		if result == nil {
			t.Fatal("ValidateParams() returned nil")
		}

		if limit := result.GetInt("limit"); limit != 100 {
			t.Errorf("limit = %d, want 100 (default)", limit)
		}
		if verbose := result.GetBool("verbose"); verbose {
			t.Error("verbose should be false (default)")
		}
		if format := result.GetString("format"); format != "json" {
			t.Errorf("format = %q, want 'json' (default)", format)
		}
	})

}

// ============================================================================
// More Response Helper Tests
// ============================================================================

func TestRespondSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	respondSuccess(c, "operation completed", gin.H{"id": "123"})

	// respondSuccess returns 200 OK
	if status := c.Writer.Status(); status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "success") {
		t.Error("response should contain 'success'")
	}
}

// ============================================================================
// Validation Rule Builder Tests (Extended)
// ============================================================================

func TestRequiredQueryParam(t *testing.T) {
	rule := RequiredQueryParam("search")
	if rule.Name != "search" {
		t.Errorf("Name = %q, want 'search'", rule.Name)
	}
	if rule.Source != ParamSourceQuery {
		t.Error("Source should be ParamSourceQuery")
	}
	if !rule.Required {
		t.Error("Required should be true")
	}
	if rule.MinLength != 1 {
		t.Errorf("MinLength = %d, want 1", rule.MinLength)
	}
}

// ============================================================================
// Mock Providers for ModelService Tests
// ============================================================================

type mockModelCache struct {
	models []*cache.CachedModel
	err    error
}

func (m *mockModelCache) ListModels(ctx context.Context, host, repo, app, model string) ([]*cache.CachedModel, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.models, nil
}

func (m *mockModelCache) LookupModel(ctx context.Context, name string) (*cache.CachedModel, error) {
	if m.err != nil {
		return nil, m.err
	}
	for _, cm := range m.models {
		if cm != nil && cm.Name == name {
			return cm, nil
		}
	}
	return nil, cache.ErrModelNotFound{ModelName: name}
}

func (m *mockModelCache) Invalidate() {}

func (m *mockModelCache) RouteToModelOrBroadcast(ctx context.Context, modelName, path, method string, body []byte) (*routing.Response, error) {
	return nil, nil
}

func (m *mockModelCache) TargetNodeForModel(modelName string) string {
	for _, cm := range m.models {
		if cm != nil && cm.Name == modelName {
			return cm.Node
		}
	}
	return ""
}

func (m *mockModelCache) IsLocalNode(host string) bool {
	return host == "localhost" || host == ""
}

type mockModelRegistry struct{}

func (m *mockModelRegistry) GetModelRegistry() *modelregistry.Registry {
	return nil
}

func (m *mockModelRegistry) RescanLocal() (map[string]any, error) {
	return map[string]any{"scanned": true}, nil
}

type mockInstanceProvider struct{}

func (m *mockInstanceProvider) GetInstanceByModel(modelName string) (*instance.Instance, bool) {
	return nil, false
}

// ============================================================================
// ModelService Tests
// ============================================================================

func TestNewModelSearchService(t *testing.T) {
	t.Run("constructor returns non-nil", func(t *testing.T) {
		svc := NewModelSearchService(nilAppsConfig, nil)
		if svc == nil {
			t.Fatal("NewModelSearchService() returned nil")
		}
	})

	t.Run("Start/Stop idempotent", func(t *testing.T) {
		svc := NewModelSearchService(nilAppsConfig, nil)
		ctx := context.Background()
		// Double Start, double Stop, mixed ordering must all be safe.
		svc.Start(ctx)
		svc.Start(ctx)
		svc.Stop(ctx)
		svc.Stop(ctx)
	})

	t.Run("Start/Stop nil-receiver safe", func(t *testing.T) {
		var svc *ModelSearchService
		ctx := context.Background()
		// Defensive: gates the Server lifecycle path that runs even when
		// the search service was never constructed.
		svc.Start(ctx)
		svc.Stop(ctx)
	})
}

func TestModelService_ListModels(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		mc := &mockModelCache{
			models: []*cache.CachedModel{
				{Name: "llama2-7b", Node: "node1", Provider: "ollama"},
			},
		}
		svc := NewModelService(mc, &mockModelRegistry{}, &mockNodeInfo{name: "test"}, &mockInstanceProvider{}, &mockRouter{}, nilAppsConfig)

		models, err := svc.ListModels(context.Background(), &ListModelsRequest{})
		if err != nil {
			t.Fatalf("ListModels() error = %v", err)
		}
		if len(models) != 1 {
			t.Errorf("len(models) = %d, want 1", len(models))
		}
	})

	t.Run("with refresh", func(t *testing.T) {
		mc := &mockModelCache{
			models: []*cache.CachedModel{},
		}
		svc := NewModelService(mc, &mockModelRegistry{}, &mockNodeInfo{name: "test"}, &mockInstanceProvider{}, &mockRouter{}, nilAppsConfig)

		models, err := svc.ListModels(context.Background(), &ListModelsRequest{Refresh: true})
		if err != nil {
			t.Fatalf("ListModels() error = %v", err)
		}
		if len(models) != 0 {
			t.Errorf("len(models) = %d, want 0", len(models))
		}
	})

	t.Run("cache error", func(t *testing.T) {
		mc := &mockModelCache{
			err: fmt.Errorf("cache unavailable"),
		}
		svc := NewModelService(mc, &mockModelRegistry{}, &mockNodeInfo{name: "test"}, &mockInstanceProvider{}, &mockRouter{}, nilAppsConfig)

		_, err := svc.ListModels(context.Background(), &ListModelsRequest{})
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestModelService_ShowModel(t *testing.T) {
	t.Run("model not found", func(t *testing.T) {
		mc := &mockModelCache{
			models: []*cache.CachedModel{},
		}
		svc := NewModelService(mc, &mockModelRegistry{}, &mockNodeInfo{name: "test"}, &mockInstanceProvider{}, &mockRouter{}, nilAppsConfig)

		_, err := svc.ShowModel(context.Background(), &ShowModelRequest{ModelName: "nonexistent"})
		if err == nil {
			t.Error("expected error, got nil")
		}
		if _, ok := err.(cache.ErrModelNotFound); !ok {
			t.Errorf("expected cache.ErrModelNotFound, got %T", err)
		}
	})

	t.Run("multiple matches", func(t *testing.T) {
		mc := &mockModelCache{
			models: []*cache.CachedModel{
				{Name: "llama2-7b", Node: "node1"},
				{Name: "llama2-7b", Node: "node2"},
			},
		}
		svc := NewModelService(mc, &mockModelRegistry{}, &mockNodeInfo{name: "test"}, &mockInstanceProvider{}, &mockRouter{}, nilAppsConfig)

		_, err := svc.ShowModel(context.Background(), &ShowModelRequest{ModelName: "llama2"})
		if err == nil {
			t.Error("expected error, got nil")
		}
		if _, ok := err.(ErrMultipleMatches); !ok {
			t.Errorf("expected ErrMultipleMatches, got %T", err)
		}
	})

	t.Run("single match routes to host", func(t *testing.T) {
		mc := &mockModelCache{
			models: []*cache.CachedModel{
				{Name: "llama2-7b", Node: "node1", Provider: "ollama"},
			},
		}
		router := &mockRouter{
			response: &routing.Response{
				StatusCode: 200,
				Body:       []byte(`{"modelfile":"FROM llama2"}`),
			},
		}
		svc := NewModelService(mc, &mockModelRegistry{}, &mockNodeInfo{name: "test"}, &mockInstanceProvider{}, router, nilAppsConfig)

		resp, err := svc.ShowModel(context.Background(), &ShowModelRequest{ModelName: "llama2-7b"})
		if err != nil {
			t.Fatalf("ShowModel() error = %v", err)
		}
		if resp == nil {
			t.Error("ShowModel() returned nil response")
		}
	})
}

// ============================================================================
// ShowModelResponse Tests
// ============================================================================

// ============================================================================
// Aggregation Utils Tests
// ============================================================================

func TestMatchesFilterPattern(t *testing.T) {
	tests := []struct {
		value   string
		pattern string
		want    bool
	}{
		{"llama2", "llama2", true},          // Exact match
		{"llama2", "llama*", true},          // Prefix wildcard
		{"llama2-7b", "llama*", true},       // Prefix wildcard
		{"my-llama", "*llama", true},        // Suffix wildcard
		{"my-llama-model", "*llama*", true}, // Contains
		{"mistral", "llama*", false},        // No match
		{"ollama-model", "*llama*", true},   // Contains
		{"test", "test", true},              // Exact
		{"testing", "test", false},          // No wildcard, no match
	}

	for _, tt := range tests {
		t.Run(tt.value+"_"+tt.pattern, func(t *testing.T) {
			got := matchesFilterPattern(tt.value, tt.pattern)
			if got != tt.want {
				t.Errorf("matchesFilterPattern(%q, %q) = %v, want %v", tt.value, tt.pattern, got, tt.want)
			}
		})
	}
}

// ============================================================================
// Node Utils Tests
// ============================================================================

func TestGetOSName(t *testing.T) {
	// Test returns a non-empty string
	osName := getOSName()
	if osName == "" {
		t.Error("getOSName() should not return empty string")
	}

	// Should return one of the known values or runtime.GOOS
	validNames := []string{"macOS", "Linux", "Windows"}
	found := slices.Contains(validNames, osName)
	// May also return runtime.GOOS directly for unknown platforms
	if !found && osName == "" {
		t.Errorf("getOSName() = %q, expected a valid OS name", osName)
	}
}

func TestGetActualIPAddress(t *testing.T) {
	tests := []struct {
		name       string
		configNode string
		expectIP   bool
	}{
		{
			name:       "specific IP returns same",
			configNode: "192.168.1.100",
			expectIP:   true,
		},
		{
			name:       "empty returns actual IP",
			configNode: "",
			expectIP:   true,
		},
		{
			name:       "0.0.0.0 returns actual IP",
			configNode: "0.0.0.0",
			expectIP:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := getActualIPAddress(tt.configNode)
			if tt.expectIP && ip == "" {
				t.Error("getActualIPAddress() should return non-empty IP")
			}
			// For specific IP test, verify it returns the same
			if tt.configNode == "192.168.1.100" && ip != tt.configNode {
				t.Errorf("getActualIPAddress(%q) = %q, want same IP", tt.configNode, ip)
			}
		})
	}
}
