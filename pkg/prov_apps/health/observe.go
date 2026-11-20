package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Observation holds nullable engine evidence and the cause of any missing evidence.
type Observation struct {
	Running, Waiting, KVCacheUsage *float64
	Busy                           *bool
	Cause                          ObservationCause
	StatusCode                     int
}

// ObservationCause identifies why some or all evidence is unavailable.
type ObservationCause string

const (
	ObservationUnreachable  ObservationCause = "unreachable"
	ObservationErrorStatus  ObservationCause = "error_status"
	ObservationMalformed    ObservationCause = "malformed"
	ObservationSeriesAbsent ObservationCause = "series_absent"
)

// ReadObservation samples declared evidence directly from a run's local engine.
func ReadObservation(ctx context.Context, client *http.Client, run *instance.Instance, observe *schema.Observe) Observation {
	var result Observation
	if !observesRun(observe, run) {
		return result
	}
	target := protocol.NewTarget(protocol.ProcessRunEndpoint(run.Port), backend.Engine()).Ready()
	var read func(string, bool) *float64
	if observe.Format == "prometheus" {
		body, err := target.ReadText(ctx, client, http.MethodGet, observe.Path, nil, http.StatusOK)
		if err != nil {
			return observationFailure(err)
		}
		parser := expfmt.NewTextParser(model.LegacyValidation)
		families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
		if err != nil {
			return Observation{Cause: ObservationMalformed}
		}
		read = func(name string, maximum bool) *float64 {
			return result.combineSeries(families[name], maximum)
		}
	} else {
		value, err := target.ReadJSON(ctx, client, http.MethodGet, observe.Path, nil, http.StatusOK)
		if err != nil {
			return observationFailure(err)
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return Observation{Cause: ObservationMalformed}
		}
		busy := false
		for _, name := range observe.Busy {
			field, exists := fields[name]
			queue, ok := field.([]any)
			if !exists {
				return Observation{Cause: ObservationSeriesAbsent}
			}
			if !ok {
				return Observation{Cause: ObservationMalformed}
			}
			busy = busy || len(queue) > 0
		}
		if len(observe.Busy) > 0 {
			result.Busy = &busy
		}
		read = func(name string, _ bool) *float64 {
			if field := protocol.Field(fields, name); name != "" && field != nil {
				number, _ := field.(json.Number)
				if value, err := number.Float64(); err == nil {
					return &value
				}
				result.Cause = ObservationMalformed
			}
			return nil
		}
	}
	result.Running = read(observe.Running, false)
	result.Waiting = read(observe.Waiting, false)
	result.KVCacheUsage = read(observe.KVCacheUsage, true)
	if result.Cause == "" && (observe.Running != "" && result.Running == nil ||
		observe.Waiting != "" && result.Waiting == nil || observe.KVCacheUsage != "" && result.KVCacheUsage == nil) {
		result.Cause = ObservationSeriesAbsent
	}
	return result
}

func observesRun(observe *schema.Observe, run *instance.Instance) bool {
	return observe.Applies(run.Provider, run.Resolved().Runtime)
}

func (o *Observation) combineSeries(family *dto.MetricFamily, maximum bool) *float64 {
	if len(family.GetMetric()) == 0 {
		return nil
	}
	samples, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{}, family)
	if err != nil {
		o.Cause = ObservationMalformed
		return nil
	}
	samples = slices.DeleteFunc(samples, func(sample *model.Sample) bool {
		return string(sample.Metric[model.MetricNameLabel]) != family.GetName()
	})
	if len(samples) == 0 {
		return nil
	}
	result := float64(samples[0].Value)
	for _, sample := range samples[1:] {
		if maximum {
			result = max(result, float64(sample.Value))
		} else {
			result += float64(sample.Value)
		}
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		o.Cause = ObservationMalformed
		return nil
	}
	return &result
}

func observationFailure(err error) Observation {
	if status, ok := errors.AsType[protocol.ReadStatusError](err); ok {
		return Observation{Cause: ObservationErrorStatus, StatusCode: int(status)}
	}
	if errors.Is(err, protocol.ErrMalformedJSON) || errors.Is(err, utils.ErrResponseTooLarge) {
		return Observation{Cause: ObservationMalformed}
	}
	return Observation{Cause: ObservationUnreachable}
}
