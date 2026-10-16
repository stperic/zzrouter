package process

// UnknownCause identifies the next evidence or action needed to establish fit.
type UnknownCause string

// Closed causes are shared by local sizing and node assessment boundaries.
const (
	WeightsUnavailable        UnknownCause = "weights_unavailable"
	MetadataInvalid           UnknownCause = "metadata_invalid"
	ModelUnmodelled           UnknownCause = "model_unmodelled"
	KVDtypeUnsupported        UnknownCause = "kv_dtype_unsupported"
	ContextUnresolved         UnknownCause = "context_unresolved"
	ContextBelowMinimum       UnknownCause = "context_below_minimum"
	NativeContextAdaptation   UnknownCause = "native_context_adaptation"
	BudgetNotSized            UnknownCause = "budget_not_sized"
	MemoryEvidenceUnavailable UnknownCause = "memory_evidence_unavailable"
	AssessmentFailed          UnknownCause = "assessment_failed"
	NodeOutdated              UnknownCause = "node_outdated"
	NodeUnreachable           UnknownCause = "node_unreachable"
	ProviderNotEligible       UnknownCause = "provider_not_eligible"
)

// SetUnknown retains independent evidence and adds canonical next actions to detail.
func (assessment *MemoryAssessment) SetUnknown(cause UnknownCause, detail string) {
	assessment.Status, assessment.Placement, assessment.UnknownCause = "unknown", "", cause
	assessment.Reason = cause.Reason()
	if detail != "" {
		assessment.Reason = detail + ". " + assessment.Reason
	}
}

// Reason describes the evidence gap and its next task tools.
func (cause UnknownCause) Reason() string {
	switch cause {
	case WeightsUnavailable:
		return "Local weights are absent or incomplete on this node; inspect zz_models, download or repair with zz_model_download, follow zz_job, then use zz_run_preview."
	case MetadataInvalid:
		return "Sizing metadata is missing or invalid; repair with zz_model_download and zz_job, or select another build through zz_models before zz_run_preview."
	case ModelUnmodelled:
		return "The model allocation is not modelled; use zz_run_start, follow zz_job, then inspect zz_runs and zz_logs for served context and engine-reported cache capacity and concurrency."
	case KVDtypeUnsupported:
		return "The selected KV cache dtype cannot be sized; inspect the KV parameter through zz_params_get and use zz_run_preview with a supported value."
	case ContextUnresolved:
		return "Context cannot be established; find the context parameter through zz_params_get and use zz_run_preview with a numeric value."
	case ContextBelowMinimum:
		return "Native context is below the declared minimum; select another model through zz_models or use zz_run_preview with a smaller numeric context from zz_params_get."
	case NativeContextAdaptation:
		return "Native context exceeds assessed memory and the engine may adapt; use zz_params_get and zz_run_preview with numeric context, or zz_run_start, zz_job and zz_runs for served context."
	case BudgetNotSized:
		return "This memory policy does not size model capacity; use zz_run_start, follow zz_job, then inspect zz_runs and zz_logs."
	case MemoryEvidenceUnavailable:
		return "Memory observation, selection or budget is unavailable or invalid; inspect zz_nodes and zz_params_get before zz_run_preview."
	case AssessmentFailed:
		return "Assessment preparation failed or was cancelled; inspect zz_providers and zz_params_get, then retry zz_run_preview."
	case NodeOutdated:
		return "The node did not supply this assessment contract; inspect zz_update_status and upgrade with zz_update_apply."
	case NodeUnreachable:
		return "The node did not reply; inspect zz_nodes before retrying the assessment."
	case ProviderNotEligible:
		return "This node has no eligible provider for the model; inspect zz_providers and zz_provider_install, locate the model with zz_models, or choose another node."
	}
	return ""
}
