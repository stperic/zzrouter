package server

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/utils"
)

// modelGroupPatchBody is the RFC 7396 Merge-Patch wire shape for
// PATCH /zzrouter/v1/model-groups/:name. Each field is captured as
// json.RawMessage so absent (nil), explicit null (delete), and
// set-value are all distinguishable.
//
// Replicas at this entry point is opaque whole-array replace per RFC
// 7396 (arrays don't recurse). Per-replica mutators live at a separate
// route.
type modelGroupPatchBody struct {
	Description json.RawMessage `json:"description,omitempty"`
	Strategy    json.RawMessage `json:"strategy,omitempty"`
	HealthCheck json.RawMessage `json:"health_check,omitempty"`
	Replicas    json.RawMessage `json:"replicas,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	TTLSeconds  json.RawMessage `json:"ttl_seconds,omitempty"`
	Owner       json.RawMessage `json:"owner,omitempty"`
}

// healthCheckPatchBody mirrors healthCheckConfig but with raw fields so
// individual keys obey the three-meaning rule (absent / null / value).
type healthCheckPatchBody struct {
	Path     json.RawMessage `json:"path,omitempty"`
	Interval json.RawMessage `json:"interval,omitempty"`
	Timeout  json.RawMessage `json:"timeout,omitempty"`
}

// applyModelGroupPatch returns a new ModelGroup with the patch applied.
// Validation against the strategy enum runs here so the closed-enum
// envelope (utils.ParamError) carries any failure. On wire-shape
// errors returns a single ParamError so the handler can wrap it.
func applyModelGroupPatch(current modelgroup.ModelGroup, raw json.RawMessage) (modelgroup.ModelGroup, *utils.ParamError) {
	out := current

	var patch modelGroupPatchBody
	if err := json.Unmarshal(raw, &patch); err != nil {
		return out, &utils.ParamError{
			Code:    string(httperr.CodeWrongType),
			Want:    "object",
			Message: "merge-patch body must be a JSON object: " + err.Error(),
		}
	}

	if len(patch.Description) > 0 {
		if isJSONNull(patch.Description) {
			out.Description = ""
		} else {
			var s string
			if err := json.Unmarshal(patch.Description, &s); err != nil {
				return current, &utils.ParamError{Key: "description", Code: string(httperr.CodeWrongType), Want: "string", Message: "description must be a string"}
			}
			out.Description = s
		}
	}

	if len(patch.Strategy) > 0 {
		if isJSONNull(patch.Strategy) {
			// Null reverts to server default (validateGroup applies
			// StrategyPriority when the value is empty).
			out.Strategy = ""
		} else {
			var s string
			if err := json.Unmarshal(patch.Strategy, &s); err != nil {
				return current, &utils.ParamError{Key: "strategy", Code: string(httperr.CodeWrongType), Want: "string", Message: "strategy must be a string"}
			}
			switch modelgroup.StrategyType(s) {
			case modelgroup.StrategyPriority, modelgroup.StrategyLeastLoad, modelgroup.StrategyFastest:
				out.Strategy = modelgroup.StrategyType(s)
			default:
				return current, &utils.ParamError{
					Key:     "strategy",
					Code:    string(httperr.CodeCoercionFailed),
					Got:     s,
					Message: "strategy must be one of priority, least-load, fastest",
				}
			}
		}
	}

	if len(patch.HealthCheck) > 0 {
		if isJSONNull(patch.HealthCheck) {
			out.HealthCheck = nil
		} else {
			merged, perr := applyHealthCheckPatch(out.HealthCheck, patch.HealthCheck)
			if perr != nil {
				return current, perr
			}
			out.HealthCheck = merged
		}
	}

	if len(patch.Replicas) > 0 {
		if isJSONNull(patch.Replicas) {
			out.Replicas = nil
		} else {
			var reps []replicaRequest
			if err := json.Unmarshal(patch.Replicas, &reps); err != nil {
				return current, &utils.ParamError{Key: "replicas", Code: string(httperr.CodeWrongType), Want: "array", Message: "replicas must be an array"}
			}
			domain, perr := replicasFromRequest(reps)
			if perr != nil {
				return current, perr
			}
			out.Replicas = domain
		}
	}

	if len(patch.Params) > 0 {
		if isJSONNull(patch.Params) {
			out.Params = nil
		} else {
			var kv map[string]json.RawMessage
			if err := json.Unmarshal(patch.Params, &kv); err != nil {
				return current, &utils.ParamError{Key: "params", Code: string(httperr.CodeWrongType), Want: "object", Message: "params must be an object"}
			}
			merged := mergeParamsMap(out.Params, kv)
			if len(merged) == 0 {
				out.Params = nil
			} else {
				out.Params = merged
			}
		}
	}

	if len(patch.TTLSeconds) > 0 {
		if isJSONNull(patch.TTLSeconds) {
			// null is the canonical "clear ExpiresAt" verb — distinct
			// from 0 which is rejected as out-of-range.
			out.ExpiresAt = time.Time{}
		} else {
			var ttl int
			if err := json.Unmarshal(patch.TTLSeconds, &ttl); err != nil {
				return current, &utils.ParamError{Key: "ttl_seconds", Code: string(httperr.CodeWrongType), Want: "int", Message: "ttl_seconds must be an integer"}
			}
			// PATCH rejects ttl_seconds:0 — use null to clear.
			// validateTTLAndOwner short-circuits on ttl==0, so guard explicitly.
			if ttl == 0 {
				return current, &utils.ParamError{
					Key:     "ttl_seconds",
					Code:    string(httperr.CodeOutOfRange),
					Got:     0,
					Min:     ttlSecondsMin,
					Max:     ttlSecondsMax,
					Message: "ttl_seconds must be between 60 and 86400; use null to clear",
				}
			}
			if perr := validateTTLAndOwner(ttl, ""); perr != nil {
				return current, perr
			}
			// Extend from now (not from prior ExpiresAt). User
			// expectation is "live for the next ttl seconds."
			out.ExpiresAt = utils.NowUTC().Add(time.Duration(ttl) * time.Second)
		}
	}

	if len(patch.Owner) > 0 {
		if isJSONNull(patch.Owner) {
			out.Owner = ""
		} else {
			var s string
			if err := json.Unmarshal(patch.Owner, &s); err != nil {
				return current, &utils.ParamError{Key: "owner", Code: string(httperr.CodeWrongType), Want: "string", Message: "owner must be a string"}
			}
			if perr := validateTTLAndOwner(0, s); perr != nil {
				return current, perr
			}
			out.Owner = s
		}
	}

	return out, nil
}

// applyHealthCheckPatch performs RFC 7396 merge on the health_check
// sub-object. A null at any leaf clears that field; an absent leaf
// preserves the prior value. Returns nil on error so misuse of the
// stale pointer is obvious.
func applyHealthCheckPatch(current *modelgroup.HealthCheckConfig, raw json.RawMessage) (*modelgroup.HealthCheckConfig, *utils.ParamError) {
	var patch healthCheckPatchBody
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, &utils.ParamError{Key: "health_check", Code: string(httperr.CodeWrongType), Want: "object", Message: "health_check must be an object: " + err.Error()}
	}
	out := modelgroup.HealthCheckConfig{}
	if current != nil {
		out = *current
	}

	if len(patch.Path) > 0 {
		if isJSONNull(patch.Path) {
			out.Path = ""
		} else {
			var s string
			if err := json.Unmarshal(patch.Path, &s); err != nil {
				return nil, &utils.ParamError{Key: "health_check.path", Code: string(httperr.CodeWrongType), Want: "string", Message: "health_check.path must be a string"}
			}
			out.Path = s
		}
	}
	if perr := patchDurationField(patch.Interval, "health_check.interval", &out.Interval); perr != nil {
		return nil, perr
	}
	if perr := patchDurationField(patch.Timeout, "health_check.timeout", &out.Timeout); perr != nil {
		return nil, perr
	}
	return &out, nil
}

// replicasFromRequest converts wire-shape replicaRequest into the
// domain Replica. Returns a ParamError on any per-replica field-level
// failure so the patch fails atomically — partial conversion is
// discarded by the caller.
func replicasFromRequest(reps []replicaRequest) ([]modelgroup.Replica, *utils.ParamError) {
	out := make([]modelgroup.Replica, 0, len(reps))
	for i, d := range reps {
		rep := modelgroup.Replica{
			Name:              d.Name,
			Model:             d.Model,
			App:               d.Provider,
			Node:              d.Node,
			Priority:          d.Priority,
			OnDemand:          d.OnDemand,
			GPUMemoryRequired: d.GPUMemoryRequired,
			MaxRetries:        d.MaxRetries,
			Tags:              d.Tags,
			Params:            d.Params,
		}
		if d.Timeout != "" {
			parsed, err := time.ParseDuration(d.Timeout)
			if err != nil {
				return nil, &utils.ParamError{
					Key:     fmt.Sprintf("replicas[%d].timeout", i),
					Code:    string(httperr.CodeCoercionFailed),
					Got:     d.Timeout,
					Message: fmt.Sprintf("replicas[%d] (%q) has invalid timeout: %s", i, d.Name, err.Error()),
				}
			}
			rep.Timeout = modelgroup.Duration{Duration: parsed}
		}
		out = append(out, rep)
	}
	return out, nil
}

// replicaPatchBody is the RFC 7396 wire shape for
// PATCH /zzrouter/v1/model-groups/:name/replicas/:replica. Name is
// fixed by the URL and absent from the body.
type replicaPatchBody struct {
	Model             json.RawMessage `json:"model,omitempty"`
	Provider          json.RawMessage `json:"provider,omitempty"`
	Node              json.RawMessage `json:"node,omitempty"`
	Priority          json.RawMessage `json:"priority,omitempty"`
	Timeout           json.RawMessage `json:"timeout,omitempty"`
	OnDemand          json.RawMessage `json:"on_demand,omitempty"`
	GPUMemoryRequired json.RawMessage `json:"gpu_memory_required,omitempty"`
	MaxRetries        json.RawMessage `json:"max_retries,omitempty"`
	Tags              json.RawMessage `json:"tags,omitempty"`
	Params            json.RawMessage `json:"params,omitempty"`
}

// applyReplicaPatch returns a new Replica with the patch applied. The
// caller is responsible for preserving the Name (URL identity wins).
func applyReplicaPatch(current modelgroup.Replica, raw json.RawMessage) (modelgroup.Replica, *utils.ParamError) {
	out := current

	var patch replicaPatchBody
	if err := json.Unmarshal(raw, &patch); err != nil {
		return current, &utils.ParamError{Code: string(httperr.CodeWrongType), Want: "object", Message: "merge-patch body must be a JSON object: " + err.Error()}
	}

	if perr := patchStringField(patch.Model, "model", &out.Model); perr != nil {
		return current, perr
	}
	if perr := patchStringField(patch.Provider, "provider", &out.App); perr != nil {
		return current, perr
	}
	if perr := patchStringField(patch.Node, "node", &out.Node); perr != nil {
		return current, perr
	}
	if perr := patchStringField(patch.GPUMemoryRequired, "gpu_memory_required", &out.GPUMemoryRequired); perr != nil {
		return current, perr
	}
	if perr := patchIntField(patch.Priority, "priority", &out.Priority); perr != nil {
		return current, perr
	}
	if perr := patchIntField(patch.MaxRetries, "max_retries", &out.MaxRetries); perr != nil {
		return current, perr
	}
	if perr := patchBoolField(patch.OnDemand, "on_demand", &out.OnDemand); perr != nil {
		return current, perr
	}
	if perr := patchDurationField(patch.Timeout, "timeout", &out.Timeout); perr != nil {
		return current, perr
	}

	if len(patch.Tags) > 0 {
		if isJSONNull(patch.Tags) {
			out.Tags = nil
		} else {
			var tags []string
			if err := json.Unmarshal(patch.Tags, &tags); err != nil {
				return current, &utils.ParamError{Key: "tags", Code: string(httperr.CodeWrongType), Want: "string-array", Message: "tags must be an array of strings"}
			}
			out.Tags = tags
		}
	}
	if len(patch.Params) > 0 {
		if isJSONNull(patch.Params) {
			out.Params = nil
		} else {
			var kv map[string]json.RawMessage
			if err := json.Unmarshal(patch.Params, &kv); err != nil {
				return current, &utils.ParamError{Key: "params", Code: string(httperr.CodeWrongType), Want: "object", Message: "params must be an object"}
			}
			merged := mergeParamsMap(out.Params, kv)
			if len(merged) == 0 {
				out.Params = nil
			} else {
				out.Params = merged
			}
		}
	}
	return out, nil
}

func patchStringField(raw json.RawMessage, key string, dest *string) *utils.ParamError {
	if len(raw) == 0 {
		return nil
	}
	if isJSONNull(raw) {
		*dest = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "string", Message: key + " must be a string"}
	}
	*dest = s
	return nil
}

func patchIntField(raw json.RawMessage, key string, dest *int) *utils.ParamError {
	if len(raw) == 0 {
		return nil
	}
	if isJSONNull(raw) {
		*dest = 0
		return nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil || n != float64(int64(n)) {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "int", Message: key + " must be an integer"}
	}
	*dest = int(n)
	return nil
}

func patchBoolField(raw json.RawMessage, key string, dest *bool) *utils.ParamError {
	if len(raw) == 0 {
		return nil
	}
	if isJSONNull(raw) {
		*dest = false
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "bool", Message: key + " must be a boolean"}
	}
	*dest = b
	return nil
}

func patchDurationField(raw json.RawMessage, key string, dest *modelgroup.Duration) *utils.ParamError {
	if len(raw) == 0 {
		return nil
	}
	if isJSONNull(raw) {
		dest.Duration = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "string", Message: key + " must be a duration string"}
	}
	if s == "" {
		dest.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeCoercionFailed), Got: s, Message: fmt.Sprintf("invalid %s: %s", key, err.Error())}
	}
	dest.Duration = parsed
	return nil
}

// mergeParamsMap applies RFC 7396 per-key on the extensible params map.
// Null at a key deletes it; any other value replaces it. Returns a
// fresh map so the caller never mutates current in place. RawMessages
// passed in have been pre-validated by the outer json.Unmarshal.
func mergeParamsMap(current map[string]any, patch map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(current)+len(patch))
	for k, v := range current {
		out[k] = v
	}
	for k, raw := range patch {
		if isJSONNull(raw) {
			delete(out, k)
			continue
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		out[k] = v
	}
	return out
}
