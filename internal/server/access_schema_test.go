package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pin gin's static-over-param precedence so the comments in the
// keys/teams controllers don't quietly become wrong on a gin upgrade.
func TestAccessSchema_GinStaticBeatsParamRegardlessOfOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		name string
		fn   func(*gin.Engine)
	}{
		{"static-first", func(r *gin.Engine) {
			r.GET("/keys/schema", func(c *gin.Context) { c.String(200, "schema") })
			r.GET("/keys/:id", func(c *gin.Context) { c.String(200, "id="+c.Param("id")) })
		}},
		{"param-first", func(r *gin.Engine) {
			r.GET("/keys/:id", func(c *gin.Context) { c.String(200, "id="+c.Param("id")) })
			r.GET("/keys/schema", func(c *gin.Context) { c.String(200, "schema") })
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := gin.New()
			scenario.fn(r)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/keys/schema", nil))
			assert.Equalf(t, "schema", w.Body.String(), "registration order %q broke /keys/schema resolution", scenario.name)
		})
	}
}

func TestKeysSchema_EndpointResponds(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", "/zzrouter/v1/keys/schema", TestAdminKey, nil)
	require.Equalf(t, http.StatusOK, resp.Code, "body=%s", string(resp.Body))

	var env struct {
		Success bool                 `json:"success"`
		Message string               `json:"message"`
		Data    accessSchemaResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	assert.True(t, env.Success)
	assert.NotEmpty(t, env.Data.Create)
	assert.NotEmpty(t, env.Data.Update)
	assert.True(t, env.Data.Create["name"].Required, "name should be required on create")
	assert.False(t, env.Data.Update["name"].Required, "PATCH fields are never required")
	assert.ElementsMatch(t, keyRoleEnum(), env.Data.Create["role"].Enum)
}

func TestTeamsSchema_EndpointResponds(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", "/zzrouter/v1/teams/schema", TestAdminKey, nil)
	require.Equalf(t, http.StatusOK, resp.Code, "body=%s", string(resp.Body))

	var env struct {
		Success bool                 `json:"success"`
		Message string               `json:"message"`
		Data    accessSchemaResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	assert.True(t, env.Success)
	assert.NotEmpty(t, env.Data.Create)
	assert.NotEmpty(t, env.Data.Update)
	assert.True(t, env.Data.Create["id"].Required)
	assert.True(t, env.Data.Create["name"].Required)
	assert.ElementsMatch(t, resetPeriodEnum(), env.Data.Create["reset_period"].Enum)
}

// Reflection-based drift catches both directions: forgetting to
// advertise a new field, and leaving a stale entry behind.
func TestKeysSchema_NoFieldDrift(t *testing.T) {
	assertSchemaMatchesStruct(t, "keys.create", reflect.TypeOf(createKeyHTTPRequest{}), keysSchema().Create)
	assertSchemaMatchesStruct(t, "keys.update", reflect.TypeOf(UpdateKeyRequest{}), keysSchema().Update)
}

func TestTeamsSchema_NoFieldDrift(t *testing.T) {
	assertSchemaMatchesStruct(t, "teams.create", reflect.TypeOf(CreateTeamRequest{}), teamsSchema().Create)
	assertSchemaMatchesStruct(t, "teams.update", reflect.TypeOf(UpdateTeamRequest{}), teamsSchema().Update)
}

// Reflection on the actual handler-bound type — production drift in
// `binding:"required"` would change createKeyHTTPRequest and break
// this assertion immediately. A test-local copy of the struct would
// silently rot.
func TestAccessSchema_RequiredFlagsMatchBindingTags(t *testing.T) {
	for resource, pair := range map[string]struct {
		typ    reflect.Type
		fields map[string]accessFieldDTO
	}{
		"keys.create":  {reflect.TypeOf(createKeyHTTPRequest{}), keysSchema().Create},
		"teams.create": {reflect.TypeOf(CreateTeamRequest{}), teamsSchema().Create},
	} {
		for _, name := range requiredJSONFields(pair.typ) {
			f, ok := pair.fields[name]
			require.Truef(t, ok, "%s: required field %q missing from schema", resource, name)
			assert.Truef(t, f.Required, "%s: field %q is binding:required but Required=false in schema", resource, name)
		}
	}
}

// Type-fidelity: the schema's "type" string must agree with the Go
// kind of the underlying field. An agent that sees "string" for what's
// actually *int gets misinformed.
func TestAccessSchema_TypeFidelity(t *testing.T) {
	cases := []struct {
		label  string
		typ    reflect.Type
		fields map[string]accessFieldDTO
	}{
		{"keys.create", reflect.TypeOf(createKeyHTTPRequest{}), keysSchema().Create},
		{"keys.update", reflect.TypeOf(UpdateKeyRequest{}), keysSchema().Update},
		{"teams.create", reflect.TypeOf(CreateTeamRequest{}), teamsSchema().Create},
		{"teams.update", reflect.TypeOf(UpdateTeamRequest{}), teamsSchema().Update},
	}
	for _, c := range cases {
		for name, want := range goJSONTypeMap(c.typ) {
			f, ok := c.fields[name]
			require.Truef(t, ok, "%s: drift — schema missing field %q", c.label, name)
			assert.Equalf(t, want, f.Type, "%s: field %q schema type=%q but Go type implies %q", c.label, name, f.Type, want)
		}
	}
}

// goJSONTypeMap returns a name→declared-type map for every JSON-tagged
// field on typ. Pointer + slice + map kinds are unwrapped to the base
// kind; output strings match the schema's "type" vocabulary
// (string/int/float/bool/object<...>/array<...>).
func goJSONTypeMap(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range goJSONTypeMap(ft) {
					out[k] = v
				}
				continue
			}
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			continue
		}
		out[name] = goKindToSchemaType(f.Type)
	}
	return out
}

func goKindToSchemaType(t reflect.Type) string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice, reflect.Array:
		return "array<" + goKindToSchemaType(t.Elem()) + ">"
	case reflect.Map:
		return "object<" + goKindToSchemaType(t.Key()) + "," + goKindToSchemaType(t.Elem()) + ">"
	}
	return t.Kind().String()
}

// assertSchemaMatchesStruct fails if the JSON-tagged fields of typ and
// the keys of fields don't match exactly. Direction matters: missing in
// schema = forgot to advertise; missing in struct = stale schema entry.
func assertSchemaMatchesStruct(t *testing.T, label string, typ reflect.Type, fields map[string]accessFieldDTO) {
	t.Helper()
	want := jsonFieldsOf(typ)
	for _, f := range want {
		_, ok := fields[f]
		assert.Truef(t, ok, "%s: field %q on request struct is missing from schema", label, f)
	}
	wantSet := map[string]struct{}{}
	for _, f := range want {
		wantSet[f] = struct{}{}
	}
	for f := range fields {
		_, ok := wantSet[f]
		assert.Truef(t, ok, "%s: schema declares field %q that does not exist on request struct", label, f)
	}
}

// jsonFieldsOf returns every JSON tag on the struct, recursing into
// embedded struct fields (the controller's create wrapper embeds
// CreateKeyRequest).
func jsonFieldsOf(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, jsonFieldsOf(ft)...)
				continue
			}
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func requiredJSONFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, requiredJSONFields(ft)...)
				continue
			}
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "" || jsonTag == "-" {
			continue
		}
		bindingTag := f.Tag.Get("binding")
		if !strings.Contains(bindingTag, "required") {
			continue
		}
		name := strings.SplitN(jsonTag, ",", 2)[0]
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// The OpenAPI request schemas describe the same bodies GET /keys/schema
// and /teams/schema do, which the tests above tie to the structs the
// handlers bind. An agent reading either must be told the same fields
// and the same required set.
func TestAccessSchema_OpenAPIMatches(t *testing.T) {
	schemas, ok := mapAt(loadSpecTree(t)["components"], "schemas")
	require.True(t, ok, "openapi.yaml has no components.schemas")
	for component, fields := range map[string]map[string]accessFieldDTO{
		"CreateKeyRequest":  keysSchema().Create,
		"UpdateKeyRequest":  keysSchema().Update,
		"CreateTeamRequest": teamsSchema().Create,
		"UpdateTeamRequest": teamsSchema().Update,
	} {
		spec, ok := mapAt(schemas, component)
		require.Truef(t, ok, "openapi.yaml has no %s", component)
		props, _ := mapAt(spec, "properties")
		var specFields, wantFields, specRequired, wantRequired []string
		for name := range props {
			specFields = append(specFields, name)
		}
		for name, f := range fields {
			wantFields = append(wantFields, name)
			if f.Required {
				wantRequired = append(wantRequired, name)
			}
		}
		if req, ok := spec["required"].([]any); ok {
			for _, r := range req {
				specRequired = append(specRequired, r.(string))
			}
		}
		assert.ElementsMatchf(t, wantFields, specFields, "%s: properties", component)
		assert.ElementsMatchf(t, wantRequired, specRequired, "%s: required", component)
	}
}
