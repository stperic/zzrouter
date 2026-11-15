import { el, text, button, leaveButton, heading, actions, field, input, select, facts, message, problem, selectableRow, markSelection, badge } from './dom.mjs';
import { details } from './details.mjs';
import { modelKey, modelIdentity, modelQuery, modelInferenceTarget, bytes } from './model-data.mjs';
import { providerName } from './provider-icons.mjs';
import { discoverModels } from './model-discovery.mjs';
import { parameterSettings } from './parameters.mjs';
import { startRun } from './run-form.mjs';

export function modelTabs(p, mode) {
  return el('nav', { class: 'actions tabs', 'aria-label': 'Model views' }, [['available', 'Available'], ['discover', 'Discover']].map(([id, label]) => {
    const control = leaveButton(label, () => p.go('models', undefined, id), p.api);
    control.setAttribute('aria-current', String(id === mode)); return control;
  }));
}

export async function models(p, selected, mode = 'available') {
  if (mode === 'discover') {
    p.root.append(heading('Models', 'Find models and choose exactly where to deploy them.'), modelTabs(p, mode));
    return discoverModels(p, selected);
  }
  const search = input('model_search', '', { type: 'search', placeholder: 'Search available models' });
  const node = select('model_node', [['', 'All nodes']], '');
  const provider = select('model_provider', [['', 'All providers']], '');
  const feedback = el('div', { 'aria-live': 'polite' }), results = el('div');
  const list = el('section', { class: 'card entity-list' }, el('div', { class: 'workload-filters' }, field('Search', search), field('Compute node', node), field('Provider', provider)), feedback, results);
  const workspace = el('div', { class: 'entity-workspace' }, list);
  let records = [], drawer, reading = false;
  const choose = async key => {
    drawer?.dispose(!key); drawer = undefined; selected = key; markSelection(results, selected);
    if (!key) return;
    drawer = details({ ...p, root: workspace }, 'Model details', () => p.go('models'));
    const model = records.find(item => modelKey(item) === key);
    if (!model) { drawer.root.append(message('This exact model is not in the current inventory. Refresh to check again.', 'warning')); return; }
    await showModel(drawer, model, read);
  };
  function render() {
    const query = search.value.trim().toLowerCase();
    const rows = records.filter(item => (!node.value || item.node === node.value) && (!provider.value || item.assigned_app === provider.value) && `${item.name} ${item.source_id || ''} ${item.source_repo} ${item.assigned_app} ${item.node}`.toLowerCase().includes(query));
    results.replaceChildren(rows.length ? el('div', { class: 'table-wrap' }, el('table', {},
      el('thead', {}, el('tr', {}, ['Model', 'Provider', 'Node', 'Size'].map(label => el('th', { scope: 'col' }, label)))),
      el('tbody', {}, rows.map(item => {
        const key = modelKey(item), open = () => p.go('models', key);
        return selectableRow(key, selected, open, el('td', {}, leaveButton(item.name, open, p.api, 'link entity-title'), item.incomplete ? badge('Incomplete', 'warning') : null, text([item.source_id, item.format, item.quantization].filter(Boolean).join(' · '), 'muted')),
          el('td', {}, providerName(item.assigned_app, item.assigned_app || 'Not reported')), el('td', {}, item.is_cloud ? 'Cloud' : item.node || 'Not reported'), el('td', {}, item.size > 0 ? bytes(item.size) : 'Not reported'));
      })))) : message(records.length ? 'No models match these filters.' : 'No available models were reported. Use Discover to find models.'), text(`${rows.length} models shown`, 'muted'));
  }
  async function read() {
    if (reading) return;
    reading = true;
    try {
      const next = await p.api.list('/models?refresh=true', { signal: p.scope.signal, timeoutMs: 45000 });
      if (!p.current()) return;
      records = next;
      for (const [control, key, label] of [[node, 'node', 'All nodes'], [provider, 'assigned_app', 'All providers']]) {
        const value = control.value;
        control.replaceChildren(el('option', { value: '' }, label), ...[...new Set(records.map(item => item[key]).filter(Boolean))].sort().map(value => el('option', { value }, value)));
        control.value = value;
      }
      feedback.replaceChildren(); render();
    } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
    finally { reading = false; }
  }
  p.root.append(heading('Models', 'Inspect available models or discover models to add to your nodes.', leaveButton('Refresh', read, p.api), leaveButton('Discover models', () => p.go('models', undefined, 'discover'), p.api, 'primary')), modelTabs(p, 'available'), workspace);
  for (const control of [search, node, provider]) control.addEventListener(control === search ? 'input' : 'change', render);
  await read(); if (!p.current()) return;
  p.handleSelection?.(choose, (_key, nextMode) => !nextMode || nextMode === 'available');
  if (selected) await choose(selected);
}

export async function showModel(p, model, refresh) {
  const show = async () => {
    const owner = p.panel.view('Model details');
    const target = modelInferenceTarget(model);
    const controls = actions(leaveButton('Chat', () => p.go('chat', target.model, target.node), p.api, 'primary'), leaveButton('Test model', () => testModel(owner.panel.view('Test model'), model, show), p.api));
    if (!model.is_cloud && model.node) controls.append(leaveButton('Start run', () => startRun(owner.panel.view('Start run'), [{ name: model.node }], model.node, { model: model.name, provider: model.assigned_app }), p.api));
    if (model.assigned_app) controls.append(leaveButton('Parameters', () => parameterSettings(owner.panel.view('Model parameters'), { provider: model.assigned_app, node: target.node || '', model: model.name, back: show }), p.api));
    owner.root.append(el('h2', {}, model.name), controls, facts([['Provider', model.assigned_app || 'Not reported'], ['Node', model.is_cloud ? 'Cloud' : model.node], ['Registry', model.source_repo || 'Not reported'], ['Source', model.source_id || 'Not reported'], ['Size', model.size > 0 ? bytes(model.size) : 'Not reported'], ['Format', model.format || 'Not reported'], ['Quantization', model.quantization || 'Not reported']]));
    const diagnostic = el('div'); owner.root.append(diagnostic);
    const remove = button(model.is_cloud ? 'Remove cloud model' : 'Delete model', () => deleteModel(owner, model, refresh), 'danger');
    remove.setAttribute('data-protected-actions', '');
    owner.root.append(el('section', { class: 'access-section' }, el('h3', {}, model.is_cloud ? 'Model registration' : 'Storage'), text(model.is_cloud ? 'Remove this model from the provider’s registered models in zzRouter. Provider credentials are retained.' : 'Delete this exact model copy from this node. Stop any run using it in Workloads first.', 'muted'), remove));
    try {
      const value = await p.api.data('/models/show?' + modelQuery(model) + '&verbose=true', { signal: owner.scope.signal });
      if (!owner.current()) return;
      const info = value.model_info || value.details || value;
      const fields = Object.entries(info).filter(([key, value]) => !/template|license|system|parameter|tokenizer/i.test(key) && (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean')).slice(0, 30);
      diagnostic.replaceChildren(el('details', {}, el('summary', {}, 'Model information'), fields.length ? facts(fields) : text('No additional model information was reported.')));
    } catch (error) { if (owner.current()) diagnostic.replaceChildren(problem(error)); }
  };
  await show();
}

export async function deleteModel(p, model, refresh) {
  const source = model.source_id ? ` (source: ${model.source_id})` : '';
  const confirmation = model.is_cloud ? `Remove the registration for ${model.name} from ${model.assigned_app || model.source_repo}${source}? This removes its registration in zzRouter.` : `Delete ${model.name} from ${model.node}${source}? This removes its stored files.`;
  if (!p.current() || !p.api.canChange() || !confirm(confirmation)) return;
  const owner = p.panel.view(model.is_cloud ? 'Removal result' : 'Delete result');
  owner.root.append(message(model.is_cloud ? 'Removing the selected cloud model registration…' : 'Deleting the selected model copy…'));
  try {
    const result = await p.api.data('/models', { method: 'DELETE', body: { models: [modelIdentity(model)] }, signal: owner.scope.signal });
    if (!owner.current()) return;
    owner.root.replaceChildren(el('h2', {}, model.name), facts([['Node', model.node], ['Source', model.source_id || 'Not reported']]), message(`${result.deleted ?? 0} ${model.is_cloud ? 'model registrations removed' : 'model copies deleted'}.`, result.errors?.length ? 'warning' : ''), ...(result.errors || []).map(error => message(error, 'warning')));
  } catch (error) { if (owner.current()) owner.root.replaceChildren(el('h2', {}, model.name), facts([['Node', model.node], ['Source', model.source_id || 'Not reported']]), problem(error)); }
  if (!owner.current()) return;
  owner.root.append(text('Refresh the inventory to inspect the result. Deletion is never replayed automatically.', 'muted'), leaveButton('Refresh inventory', async () => { await refresh(); }, p.api), leaveButton('Close', p.close, p.api));
  await refresh();
}

export async function testModel(p, model, back) {
  let running = false, controller;
  const target = modelInferenceTarget(model);
  const feedback = el('div', { 'aria-live': 'polite' }), output = el('div', { 'data-sensitive': true });
  const run = button('Send test', send, 'primary'); run.setAttribute('data-protected-actions', '');
  const stop = button('Stop', () => controller?.abort()); stop.disabled = true;
  p.root.append(el('h2', {}, model.name), text('Send a short “Reply with OK.” prompt through the same inference API as Chat. This can load a local model and incur cloud usage.', 'muted'), facts([['Requested model', target.model], ['Requested node', target.node || 'Coordinator routing']]), actions(run, stop, leaveButton('Back', back, p.api)), feedback, output);
  async function send() {
    if (running || !p.current() || !p.api.canChange()) return;
    running = true; run.disabled = true; stop.disabled = false; controller = new AbortController();
    output.replaceChildren(); feedback.replaceChildren(message('Preparing test…'));
    let final;
    try {
      const { createChatAdapter } = await import('./vendor/chat-transport.mjs');
      if (!p.current()) return;
      const adapter = createChatAdapter({ baseURL: p.api.origin + '/v1', fetcher: p.api.inferenceFetch.bind(p.api), signal: p.scope.signal,
        settings: () => ({ ...target, maxTokens: 32 }), onStatus: value => { if (p.current() && value) feedback.replaceChildren(message(value)); } });
      for await (const value of adapter.run({ messages: [{ role: 'user', content: [{ type: 'text', text: 'Reply with OK.' }] }], abortSignal: controller.signal })) {
        if (!p.current()) return;
        final = value;
        output.replaceChildren(text((value.content || []).filter(part => part.type === 'text').map(part => part.text).join('')));
      }
      if (!p.current()) return;
      const stats = final?.metadata?.custom || {};
      feedback.replaceChildren(message(controller.signal.aborted ? 'Test stopped.' : final?.status?.type === 'complete' ? 'Test response completed.' : final?.status?.error || 'The test did not report a completed response.', final?.status?.type === 'complete' ? '' : 'warning'),
        facts([['Served by', [stats.provider, stats.node].filter(Boolean).join(' · ') || 'Not reported'], ['First token', Number.isFinite(stats.firstTokenMs) ? `${Math.round(stats.firstTokenMs)} ms` : 'Not reported'], ['Elapsed', Number.isFinite(stats.durationMs) ? `${Math.round(stats.durationMs)} ms` : 'Not reported'], ['Output tokens', stats.outputTokens ?? 'Not reported']]));
    } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
    finally { running = false; if (p.current()) { run.disabled = false; stop.disabled = true; } }
  }
}
