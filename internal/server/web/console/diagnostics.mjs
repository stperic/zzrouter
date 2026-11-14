import { el, text, facts, date, message, leaveButton } from './dom.mjs';

export function failureDetails(failure) {
  if (!failure) return null;
  return el('details', { class: 'diagnostic-evidence' }, el('summary', {}, 'Failure evidence'),
    facts([['Exit code', failure.exit_code], ['Signal', failure.signal || 'Not reported']]),
    el('pre', {}, (failure.error_tail || []).slice(0, 32).join('\n').slice(0, 8192)), failure.truncated ? text('The server truncated this diagnostic.', 'muted') : null);
}
export function providerDiagnostics(p, provider, node) {
  const service = provider?.service;
  return el('section', { class: 'access-section' }, el('h3', {}, 'Reported lifecycle'),
    facts([['State', provider?.state || 'Not reported'], ['Run scope', provider?.run_scope || 'Not reported']]),
    !service ? text('Service observations are unavailable.', 'muted') : [
      facts([['Supervisor', service.supervisor], ['Service', service.running ? 'Running' : 'Not running'], ['Desired state', service.desired_running ? 'Running' : 'Stopped']]),
      service.backoff ? message(service.backoff, 'warning') : null,
      service.job_id ? leaveButton('View readiness job', () => p.go('workloads', `job/${encodeURIComponent(node)}/${encodeURIComponent(service.job_id)}`, 'jobs'), p.api) : null,
      service.last_exit ? [text(`Last exit: ${date(service.last_exit.at)} · ${service.last_exit.reason}`), failureDetails(service.last_exit.detail)] : null,
    ]);
}

export const routeReason = code => ({
  provider_access_denied: 'This team is not allowed to use the provider.',
  model_access_denied: 'This team is not allowed to use the model.',
  model_unpriced: 'No price is available for the budget check.',
  route_unavailable: 'The route is expired or has no destinations.',
  cooldown: 'The destination is cooling down after a failure.',
  provider_cooldown: 'The provider is cooling down after a failure.',
  tag_mismatch: 'The destination does not match the requested tags.',
  excluded: 'The destination was explicitly excluded.',
  latency_budget: 'The destination exceeds the latency budget.',
  route_not_allowed: 'This team is not allowed to use the route.',
  provider_not_allowed: 'This team is not allowed to use the provider.',
  budget_exceeded: 'The team budget does not allow this request.',
  spend_limit_exceeded: 'The spending limit would be exceeded.',
  circuit_open: 'The destination is temporarily excluded after failures.',
  unhealthy: 'The destination is not healthy.',
  model_not_found: 'The model is not in the published catalog.',
  no_eligible_replicas: 'No destination passed the preview checks.',
})[code] || String(code || 'Reason unavailable').replaceAll('_', ' ');
