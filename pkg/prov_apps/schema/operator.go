package schema

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Declare overlays accepted operator fields, retaining each refusal at its config path.
func Declare(provider string, base map[string]ParamShape, endpointShapes map[string]YAMLEndpoint, cfg config.ServiceConfig) (map[string]ParamShape, []utils.ParamError) {
	out := maps.Clone(base)
	counts := map[string]int{}
	for key := range cfg.Schema {
		counts[runsettings.FlagName(key)]++
	}
	var problems []utils.ParamError
	for _, key := range slices.Sorted(maps.Keys(cfg.Schema)) {
		name, d := runsettings.FlagName(key), cfg.Schema[key]
		refuse := func(field string, code httperr.ParamErrorCode, message string) {
			problems = append(problems, utils.ParamError{Key: "schema." + key + field, Code: string(code), Message: message})
		}
		if !runsettings.ValidKey(key) {
			refuse("", httperr.CodeInvalidValue, "a parameter key must start with a letter and contain only letters, digits, hyphens or underscores")
			continue
		}
		if counts[name] > 1 {
			refuse("", httperr.CodeDuplicateFlag, "two declarations spell one flag; remove one spelling")
			continue
		}
		_, owned := Registry[provider][name]
		for _, feature := range cfg.Features {
			owned = owned || feature.Flag != "" && runsettings.FlagName(feature.Flag) == name
		}
		if owned || runsettings.IsParameter(name) {
			refuse("", httperr.CodeInvalidValue, "zzRouter owns this parameter's shape")
			continue
		}
		shape, shipped := base[name]
		endpoints := map[string]bool{}
		for endpoint, overlay := range endpointShapes {
			for literal := range overlay.Parameters {
				if runsettings.FlagName(literal) == name {
					endpoints[endpoint] = true
				}
			}
		}
		if !shipped && len(endpoints) > 0 {
			refuse("", httperr.CodeInvalidValue, fmt.Sprintf("parameter is shipped only under endpoints %v; operator declarations must name flat parameters", slices.Sorted(maps.Keys(endpoints))))
			continue
		}
		if d.Type != nil && shape.Kind != "" && ParamKind(*d.Type) != shape.Kind {
			refuse(".type", httperr.CodeInvalidValue, "type conflicts with the shipped parameter")
			continue
		}
		yp := YAMLParam{
			Type:        declarationValue(d.Type),
			Min:         d.Min,
			Max:         d.Max,
			Enum:        declarationValue(d.Enum),
			Description: declarationValue(d.Description),
			Pass:        declarationValue(d.Pass),
			Auto:        declarationValue(d.Auto),
			Negatable:   declarationValue(d.Negatable),
		}
		if shipped {
			for field, supplied := range map[string]bool{
				"type":      d.Type != nil,
				"pass":      d.Pass != nil,
				"auto":      d.Auto != nil,
				"negatable": d.Negatable != nil,
			} {
				if supplied {
					refuse("."+field, httperr.CodeInvalidValue, "a shipped parameter accepts only enum, min, max and description overrides")
				}
			}
			yp.Type, yp.Pass, yp.Auto, yp.Negatable = "", "", "", false
		}
		if d.Min != nil {
			shape.Min = nil
		}
		if d.Max != nil {
			shape.Max = nil
		}
		if d.Enum != nil {
			shape.Enum = nil
		}
		if d.Description != nil {
			shape.Description = ""
		}
		merged, err := mergeOne(provider, "schema."+key, yp, shape)
		if err != nil {
			field := ""
			if errors.Is(err, errInvalidRange) {
				field = ".min"
			}
			refuse(field, httperr.CodeInvalidValue, err.Error())
			continue
		}
		if !shipped && name != key {
			merged.Flag = key
		}
		merged.Operator = true
		out[name] = merged
	}
	slices.SortFunc(problems, func(a, b utils.ParamError) int { return cmp.Compare(a.Key, b.Key) })
	return out, problems
}

func declarationValue[T any](p *T) (out T) {
	if p != nil {
		return *p
	}
	return out
}
