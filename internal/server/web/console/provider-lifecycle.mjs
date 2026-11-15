import { el, text, button, leaveButton, actions, field, input, select, facts, message, problem, badge, editorActions, formFeedback, clearSecrets, date } from './dom.mjs';
import { liveReads } from './live-reads.mjs';
import { providerName } from './provider-icons.mjs';
import { jobDetails } from './workload-details.mjs';
import { newerJob, workloadKey } from './workload-data.mjs';
import { parameterSettings } from './parameters.mjs';
import { installProvider } from './provider-install.mjs';

export const providerPath = (provider, node, suffix = '') => `/providers/${encodeURIComponent(provider)}${suffix}${node ? '?node=' + encodeURIComponent(node) : ''}`;

export function providerControls(p, entry, inventory, back) {
  const nodes = inventory?.[0]?.status === 'fulfilled' ? inventory[0].value : [];
  const picker = select('provider_node', [['', 'Choose a compute node…'], ...nodes.map(node => [node.name, `${node.name}${node.health_status === 'down' ? ' (offline)' : ''}`])], '');
  const manage = leaveButton('Manage runtime', () => { if (picker.value) void providerLifecycle(p.panel.view('Manage provider'), entry, picker.value, back); }, p.api, 'primary');
  manage.disabled = true; picker.addEventListener('change', () => { manage.disabled = !picker.value; });
  return el('section', { class: 'access-section' }, el('h3', {}, 'Runtime management'), text('Install, verify, update or control this provider on one compute node.', 'muted'), field('Compute node', picker), actions(manage, leaveButton('Provider defaults', () => parameterSettings(p.panel.view('Provider parameters'), { provider: entry.key, back }), p.api)));
}

export async function providerLifecycle(p, entry, node, back) {
  const show = async () => {
    const owner = p.panel.view('Manage provider');
    const content = el('div'), feedback = el('div');
    const open = action => action(owner);
    owner.root.append(providerName(entry.key, entry.name, 'h2'), text(`Compute node: ${node}`, 'muted'),
      actions(leaveButton('Install or repair', () => open(current => installProvider(current.panel.view('Install provider'), entry, node, show)), p.api, 'primary'),
        leaveButton('Parameters', () => parameterSettings(owner.panel.view('Provider parameters'), { provider: entry.key, node, back: show }), p.api),
        leaveButton('Service', () => providerService(owner.panel.view('Provider service'), entry, node, show), p.api),
        leaveButton('Versions', () => providerVersions(owner.panel.view('Provider versions'), entry, node, show), p.api), leaveButton('Back', back, p.api)), feedback, content);
    const reads = await Promise.allSettled([p.api.data(providerPath(entry.key, node), { signal: owner.scope.signal }), p.api.data(providerPath(entry.key, node, '/environment'), { signal: owner.scope.signal, timeoutMs: 60000 })]);
    if (!owner.current()) return;
    const [current, environment] = reads;
    if (current.status === 'fulfilled') {
      const value = current.value;
      content.append(facts([['State', value.state || value.status || 'Not reported'], ['Installed', value.installed ? 'Yes' : 'No'], ['Managed', value.managed ? 'Yes' : 'No'], ['Enabled', value.enabled ? 'Yes' : 'No'], ['Version', value.version || 'Not reported']]));
      const toggle = button(value.enabled ? 'Disable on this node' : 'Enable on this node', () => providerOperation(owner, entry, node, { label: value.enabled ? 'Disable provider' : 'Enable provider', method: 'PATCH', body: { enabled: !value.enabled }, confirm: `${value.enabled ? 'Disable' : 'Enable'} ${entry.name} on ${node}?` }, show));
      toggle.setAttribute('data-protected-actions', ''); content.append(toggle);
      if (value.managed) { const remove = button('Uninstall from this node', () => providerOperation(owner, entry, node, { label: 'Uninstall provider', method: 'DELETE', confirm: `Uninstall ${entry.name} from ${node}? Running models may be interrupted.` }, show), 'danger'); remove.setAttribute('data-protected-actions', ''); content.append(remove); }
    } else content.append(problem(current.reason));
    if (environment.status === 'fulfilled') {
      const value = environment.value;
      content.append(el('section', { class: 'access-section' }, el('h3', {}, 'Runtime observations'), facts([['Runtime', value.runtime], ['Installed', value.installed ? 'Yes' : 'No'], ['Recipe drift', value.recipe_drift ? 'Reported' : 'Not reported'], ['Kernels', value.kernels || 'Not reported']]),
        ...(value.findings || []).map(item => el('section', { class: 'notice warning' }, el('h4', {}, item.name), facts([['Detected', item.detected], ['Required', item.required], ['Impact', item.impact]]), ...(item.human_remediation || []).map(hint => text(hint)))),
        value.installed_manifest?.selected_components?.length ? text(`Installed components: ${value.installed_manifest.selected_components.join(', ')}`) : null));
    } else feedback.append(message('Runtime observations could not be read.', 'warning'), problem(environment.reason));
    content.append(leaveButton('Refresh observations', show, p.api));
  };
  await show();
}

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
  if (result?.job_id) return followProviderJob(owner, entry, node, result, back, returnToPlan);
  owner.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), facts([['Node', node], ['Operation', operation.label]]), message('The coordinator returned a response. Inspect fresh provider observations to verify the effect.'), ...(result?.warning ? [message(result.warning, 'warning')] : []), ...(result?.error ? [message(result.error, 'error')] : []),
    ...(result?.manual_instructions ? [text(result.manual_instructions)] : []), leaveButton('Inspect provider', back, p.api));
}

async function followProviderJob(p, entry, requestedNode, accepted, back, returnToPlan) {
  const node = accepted.node || requestedNode, id = accepted.job_id;
  let latest, terminal = false;
  const content = el('div'), feedback = el('div', { 'aria-live': 'polite' });
  const pause = button('Pause updates', () => live.toggle());
  const continuePlan = returnToPlan ? button('Return to reviewed plan', () => { if (terminal && p.current() && p.api.canLeave()) returnToPlan(); }) : null;
  if (continuePlan) continuePlan.disabled = true;
  p.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), message('Operation accepted. Completion and readiness have not yet been established.'), facts([['Requested node', requestedNode], ['Accepted node', node], ['Job', id]]),
    actions(leaveButton('Refresh job', () => live.refresh(), p.api), pause, leaveButton('View job', () => p.go('workloads', workloadKey('job', node, id), 'jobs'), p.api), leaveButton('Inspect provider', back, p.api), continuePlan), feedback, content);
  const live = liveReads(p, async automatic => {
    try {
      const next = await p.api.data(`/jobs/${encodeURIComponent(id)}?node=${encodeURIComponent(node)}`, { signal: p.scope.signal });
      if (!p.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      latest = newerJob(latest, { ...next, node }); terminal = ['done', 'failed'].includes(latest.phase);
      content.replaceChildren(jobDetails(latest));
      if (terminal) feedback.replaceChildren(message(latest.phase === 'failed' ? 'The job reported failure. Inspect current provider state before choosing another action.' : 'The job reported completion. Inspect current provider state to confirm readiness.', latest.phase === 'failed' ? 'warning' : ''));
      if (continuePlan) continuePlan.disabled = !terminal;
      return true;
    } catch (error) { if (p.current()) feedback.replaceChildren(error.status === 404 ? message('This exact job is missing or no longer retained. Its outcome is unknown. Inspect the provider.', 'warning') : problem(error)); return false; }
  }, { eligible: () => !terminal && !content.contains(document.activeElement), changed: () => { pause.textContent = live.paused ? 'Resume updates' : 'Pause updates'; } });
  await live.refresh();
}

export async function providerService(p, entry, node, back) {
  const show = async () => {
    const owner = p.panel.view('Provider service'); owner.root.append(message('Reading service status…'));
    try {
      const value = await p.api.request('/zzrouter/v1' + providerPath(entry.key, node, '/service/status'), { signal: owner.scope.signal });
      if (!owner.current()) return;
      owner.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), facts([['Node', node], ['Supervisor', value.supervisor || value.manager || 'External'], ['Managed', value.managed ? 'Yes' : 'No'], ['Running', value.running ? 'Yes' : 'No'], ['Endpoint healthy', value.healthy ? 'Yes' : 'No'], ['Backoff', value.backoff || 'None reported']]),
        ...(value.last_exit ? [facts([['Last exit', value.last_exit.reason || 'Reported'], ['Last exit time', value.last_exit.at ? date(value.last_exit.at) : 'Not reported']])] : []),
        ...(value.running_models?.length ? [text(`Loaded models: ${value.running_models.join(', ')}`)] : []));
      if (value.managed) owner.root.append(actions(...[['start', 'Start service'], ['stop', 'Stop service'], ['restart', 'Restart service'], ['apply', 'Apply configuration']].map(([action, label]) => {
        const control = button(label, () => providerOperation(owner, entry, node, { label, suffix: '/service/' + action, raw: true, confirm: `${label} for ${entry.name} on ${node}? Loaded models may be interrupted.` }, show)); control.setAttribute('data-protected-actions', ''); return control;
      })));
      else owner.root.append(message('This service is managed outside zzRouter. Its status is available for inspection.'));
      owner.root.append(actions(leaveButton('Refresh status', show, p.api), leaveButton('Back', back, p.api)));
    } catch (error) { if (owner.current()) owner.root.replaceChildren(problem(error), actions(leaveButton('Retry status', show, p.api), leaveButton('Back', back, p.api))); }
  };
  await show();
}

export async function providerVersions(p, entry, node, back) {
  const show = async (refresh = false) => {
    const owner = p.panel.view('Provider versions'); owner.root.append(message('Reading versions…'));
    try {
      const report = await p.api.data(providerPath(entry.key, '', '/versions') + (refresh ? '?refresh=true' : ''), { signal: owner.scope.signal, timeoutMs: 60000 });
      if (!owner.current()) return;
      owner.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), facts([['Published latest', report.latest || 'Not reported'], ['Version pin', report.pinned || 'No pin'], ['Checked', report.checked_at ? date(report.checked_at) : 'Not reported']]), ...(report.reason ? [text(report.reason, 'muted')] : []),
        ...(report.nodes || []).map(value => el('section', { class: 'access-section' }, el('h3', {}, value.node), facts([['Installed', value.installed || 'Not reported'], ['Comparison', value.status], ['Installable latest', value.installable_latest || report.latest || 'Not reported'], ['Install source', value.installable_source || report.source?.type || 'Not reported']]))));
      const form = el('form', { class: 'entity-editor' }), draft = owner.trackDraft?.(form), feedback = el('div', { 'aria-live': 'polite' });
      const pin = input('provider_pin', report.pinned || ''), version = input('upgrade_version');
      const save = el('button', { type: 'submit', class: 'primary' }, 'Save version pin');
      const validation = formFeedback(form, feedback, key => key === 'pinned_version' ? pin : version);
      let blocked = false;
      const controls = el('fieldset', { 'data-protected-actions': true }, field('Cluster version pin', pin, 'Applies across this provider. Leave blank to clear the pin.'));
      form.append(feedback, controls, editorActions(save, () => { if (draft?.discard() !== false) back(); }, p.api));
      form.addEventListener('submit', async event => {
        event.preventDefault(); if (blocked || !owner.current() || !p.api.canChange()) return;
        blocked = true; controls.disabled = true; save.disabled = true;
        try {
          await p.api.data(providerPath(entry.key, ''), { method: 'PATCH', body: { pinned_version: pin.value.trim() }, signal: owner.scope.signal });
          if (owner.current()) { draft?.clean(); await show(); }
        } catch (error) { if (owner.current()) { validation.error(error); feedback.append(leaveButton('Inspect current versions', () => { draft?.clean(); return show(); }, p.api)); } }
      });
      const upgrade = button('Review upgrade', () => {
        if (draft?.discard() === false) return;
        draft?.clean();
        return providerOperation(owner, entry, node, { label: 'Upgrade provider', suffix: '/upgrade', body: { version: version.value.trim() }, confirm: `Upgrade ${entry.name} on ${node} to ${version.value.trim() || 'the latest installable release'}? Running models may be interrupted.` }, back);
      }); upgrade.setAttribute('data-protected-actions', '');
      owner.root.append(form, el('section', { class: 'access-section' }, el('h3', {}, `Upgrade ${node}`), field('Target version', version, 'Leave blank for the latest installable release. Upgrade does not use the configured version pin.'), actions(upgrade)), actions(leaveButton('Check upstream', () => { if (draft?.discard() !== false) return show(true); }, p.api), leaveButton('Back', () => { if (draft?.discard() !== false) back(); }, p.api)));
    } catch (error) { if (owner.current()) owner.root.replaceChildren(problem(error), actions(leaveButton('Retry versions', () => show(), p.api), leaveButton('Back', back, p.api))); }
  };
  await show();
}

export function remoteOllama(p, onSaved) {
  const form = el('form', { class: 'entity-editor' }), draft = p.trackDraft?.(form);
  const name = input('remote_name', '', { required: true }), endpoint = input('remote_endpoint', '', { type: 'url', required: true }), token = input('remote_token', '', { type: 'password', 'data-sensitive': true });
  const feedback = el('div', { 'aria-live': 'polite' }), controls = el('fieldset', { 'data-protected-actions': true }, el('h2', {}, 'Connect remote Ollama'), field('Provider name', name, 'A unique name for this external connection.'), field('Ollama endpoint', endpoint, 'The coordinator must be able to reach this address.'), field('Access token (optional)', token));
  const save = el('button', { type: 'submit', class: 'primary' }, 'Connect provider');
  const validation = formFeedback(form, feedback, key => ({ name, endpoint, token })[key]);
  let blocked = false;
  form.append(feedback, controls, editorActions(save, p.close, p.api)); p.root.append(form);
  form.addEventListener('submit', async event => {
    event.preventDefault(); if (!form.reportValidity() || blocked || !p.current() || !p.api.canChange()) return;
    blocked = true; controls.disabled = true; save.disabled = true;
    try {
      const result = await p.api.data('/providers/instances', { method: 'POST', body: { type: 'ollama-connect', name: name.value.trim(), endpoint: endpoint.value.trim(), ...(token.value ? { token: token.value } : {}) }, signal: p.scope.signal });
      clearSecrets(form);
      if (!p.current()) return;
      draft?.clean();
      const owner = p.panel.view('Provider connection result');
      owner.root.append(message('Provider connection created. Inspect the reported provider state to check availability.'), facts([['Name', result.name], ['Endpoint', result.endpoint], ['Enabled', result.enabled ? 'Yes' : 'No']]), leaveButton('Inspect provider', () => onSaved(result.name), p.api));
    } catch (error) { clearSecrets(form); if (p.current()) { validation.error(error); feedback.append(leaveButton('Inspect providers', () => { draft?.clean(); onSaved(); }, p.api)); } }
  });
}
