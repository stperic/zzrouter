import { text, button, leaveButton, actions, facts, message, problem } from './dom.mjs';
import { providerName } from './provider-icons.mjs';
import { inspectWorkload } from './workload-details.mjs';
import { workloadKey } from './workload-data.mjs';

export const providerPath = (provider, node, suffix = '') => `/providers/${encodeURIComponent(provider)}${suffix}${node ? '?node=' + encodeURIComponent(node) : ''}`;

// Each operation moves to a result view before sending, so it cannot be replayed
// by a second click or by a failed observation. Reads retain the accepted node.
export async function providerOperation(p, entry, node, operation, back, returnToPlan) {
  if (!p.current() || !p.api.canChange() || operation.confirm && !confirm(operation.confirm)) return;
  const owner = p.panel.view(operation.label);
  owner.root.append(providerName(entry.key, entry.name, 'h2'), text(`Compute node: ${node}`, 'muted'), message(`${operation.label}: waiting for the coordinator…`));
  let result;
  try {
    const path = providerPath(entry.key, node, operation.suffix);
    const options = { method: operation.method || 'POST', body: operation.body, signal: owner.scope.signal, timeoutMs: 120000 };
    result = operation.raw ? await p.api.request('/zzrouter/v1' + path, options) : await p.api.data(path, options);
  } catch (error) {
    if (owner.current()) owner.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), facts([['Requested node', node], ['Operation', operation.label]]), problem(error), text('The operation is not replayed. Inspect the provider or its jobs before another attempt.', 'muted'), actions(leaveButton('Inspect provider', back, p.api), leaveButton('Inspect node jobs', () => p.go('workloads', workloadKey('node', node, ''), 'jobs'), p.api)));
    return;
  }
  if (!owner.current()) return;
  if (result?.job_id) {
    const acceptedNode = result.node || node;
    let completed = false;
    const resume = returnToPlan ? button('Return to reviewed plan', () => {
      if (completed && owner.current() && p.api.canLeave()) returnToPlan();
    }) : null;
    if (resume) resume.disabled = true;
    owner.root.replaceChildren(providerName(entry.key, entry.name, 'h2'),
      message('Operation accepted. Inspect the provider after completion to confirm readiness.'),
      facts([['Requested node', node], ['Accepted node', acceptedNode], ['Job', result.job_id]]));
    await inspectWorkload(owner, 'jobs', acceptedNode, result.job_id, {
      controls: [leaveButton('Inspect provider', back, p.api), resume],
      observed: (_job, terminal) => { completed = terminal; if (resume) resume.disabled = !terminal; },
    });
    return;
  }
  owner.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), facts([['Node', node], ['Operation', operation.label]]), message('The coordinator returned a response. Inspect fresh provider observations to verify the effect.'), ...(result?.warning ? [message(result.warning, 'warning')] : []), ...(result?.error ? [message(result.error, 'error')] : []),
    ...(result?.manual_instructions ? [text(result.manual_instructions)] : []), leaveButton('Inspect provider', back, p.api));
}

