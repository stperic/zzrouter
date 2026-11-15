import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, heading, actions, field, input, select, facts, message, problem, badge, markSelection, editorActions, formFeedback, invalidField } from './dom.mjs';
import { details } from './details.mjs';
import { modelTabs } from './models.mjs';
import { bytes, discoveryKey, discoveryIdentity, searchPath, nextSearchPage, searchSelection, discoveryRows } from './model-data.mjs';
import { inspectDeployment } from './deployments.mjs';
import { providerCategory } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';

export async function discoverModels(p, selected) {
  const query = input('discovery_query', '', { type: 'search', placeholder: 'Search model names' });
  const provider = select('discovery_provider', [], '');
  const sort = select('discovery_sort', [['', 'Provider default']], '');
  const tags = input('discovery_tags', '', { placeholder: 'Comma-separated tags' });
  const logic = select('discovery_tag_logic', [['AND', 'Match all tags'], ['OR', 'Match any tag']], 'AND');
  const author = input('discovery_author');
  const sourceInfo = el('p', { class: 'muted' });
  const filters = el('details', {}, el('summary', {}, 'More filters'), field('Tags', tags), field('Tag matching', logic), field('Author', author));
  const feedback = el('div', { 'aria-live': 'polite' }), results = el('div', { class: 'node-grid' }), pagination = el('div', { class: 'actions' });
  const form = el('form', {}, el('div', { class: 'workload-filters' }, field('Registry or cloud provider', provider), field('Search', query), field('Sort', sort)), sourceInfo, filters, el('button', { type: 'submit', class: 'primary' }, 'Search'));
  const list = el('section', { class: 'card entity-list' }, form, feedback, results, pagination), workspace = el('div', { class: 'entity-workspace' }, list);
  const reads = new Lifetime(); p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  let sources = [], records = [], drawer, activeQuery, history = [], pageIndex = 0;
  const choose = async key => {
    drawer?.dispose(!key); drawer = undefined; selected = key; markSelection(results, selected);
    if (!key) return;
    drawer = details({ ...p, root: workspace }, 'Discover model', () => p.go('models', undefined, 'discover'));
    await discoveryDetails(drawer, discoveryIdentity(key), records.find(item => discoveryKey(item.discovery) === key)?.discovery);
  };
  const updateFilters = () => {
    const source = sources.find(item => item.name === provider.value);
    sort.replaceChildren(el('option', { value: '' }, 'Provider default'), ...[...new Set([...(source?.sort_options || []), ...(source?.client_sorts || [])])].map(value => el('option', { value }, `${value}${source.client_sorts?.includes(value) ? ' (this page)' : ''}`)));
    tags.disabled = !source?.has_tags; logic.disabled = !source?.has_tags;
    sourceInfo.textContent = [source?.description, source?.client_filter ? 'Name filtering applies to the current provider page. Browse more pages to see additional matches.' : '', source?.is_cloud && !source.configured ? 'This cloud connection is not configured yet.' : ''].filter(Boolean).join(' ');
  };
  async function read(page = {}, isNew = false) {
    if (!p.current() || !(p.canLeave?.() ?? p.api.canLeave())) return;
    const source = sources.find(item => item.name === provider.value);
    if (!source) { feedback.replaceChildren(message('Choose a model discovery provider first.')); return; }
    if (isNew) { activeQuery = searchSelection(source, { query: query.value.trim(), sort: sort.value, tags: tags.disabled ? '' : tags.value.trim(), logic: logic.value, author: author.value.trim() }); history = [{}]; pageIndex = 0; }
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    feedback.replaceChildren(message('Searching models…')); pagination.replaceChildren();
    try {
      const result = await p.api.data(searchPath({ ...activeQuery, ...page }), { signal, timeoutMs: 60000 });
      if (!p.current() || !scope.current()) return;
      records = result.results || [];
      const visible = discoveryRows(records, activeQuery);
      results.replaceChildren(...visible.map(({ discovery: item }) => {
        const key = discoveryKey(item), control = leaveButton('', () => p.go('models', key, 'discover'), p.api, 'network-item');
        control.setAttribute('data-selection', key);
        control.append(el('span', { class: 'network-item-title' }, item.display_name || item.api_id, badge(item.is_cloud ? 'Cloud' : item.provider)),
          text(item.description || item.api_id, 'muted'), text([item.size_bytes > 0 ? bytes(item.size_bytes) : '', item.downloads ? `${item.downloads.toLocaleString()} downloads` : '', item.context_length ? `${item.context_length.toLocaleString()} context` : ''].filter(Boolean).join(' · '), 'muted'));
        return control;
      }));
      markSelection(results, selected);
      feedback.replaceChildren(text(visible.length ? `${visible.length} results on this page${result.total ? ` · ${result.is_total_exact ? '' : 'about '}${result.total.toLocaleString()} total` : ''}` : 'No matching models on this page. Try another search or browse the next page when available.', 'muted'));
      if (pageIndex > 0) pagination.append(leaveButton('Previous', () => { pageIndex--; return read(history[pageIndex]); }, p.api));
      const next = nextSearchPage(result);
      if (next) pagination.append(leaveButton('Next', () => { history[++pageIndex] = next; return read(next); }, p.api));
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error), leaveButton('Retry search', () => read(page), p.api)); }
  }
  provider.addEventListener('change', () => { updateFilters(); void read({}, true); });
  form.addEventListener('submit', event => { event.preventDefault(); void read({}, true); });
  p.root.append(heading('Models', 'Find models and choose exactly where to deploy them.'), modelTabs(p, 'discover'), workspace);
  try {
    const catalog = await p.api.data('/search?type=providers', { signal: p.scope.signal });
    if (!p.current()) return;
    sources = catalog.results || [];
    provider.replaceChildren(...sources.map(item => el('option', { value: item.name }, item.display_name || item.name)));
    const initialProvider = discoveryIdentity(selected).provider;
    if (sources.some(item => item.name === initialProvider)) provider.value = initialProvider;
    updateFilters();
    p.handleSelection?.(choose, (_key, mode) => mode === 'discover');
    if (provider.value) await read({}, true);
    else feedback.replaceChildren(message('The coordinator did not declare any model discovery providers.'));
    if (selected && p.current()) await choose(selected);
  } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
}

export async function discoveryDetails(p, identity, initial) {
  const show = async () => {
    const owner = p.panel.view('Discover model');
    owner.root.append(message('Loading model card…'));
    try {
      const value = await p.api.data(`/models/card/${encodeURIComponent(identity.provider)}/${identity.api_id.split('/').map(encodeURIComponent).join('/')}`, { signal: owner.scope.signal });
      if (!owner.current()) return;
      const model = value.discovery || initial;
      if (!model) throw new Error('The coordinator returned no discovery metadata for this model.');
      owner.root.replaceChildren(el('h2', {}, model.display_name || model.api_id), providerName(model.provider, model.provider), text(model.description || '', 'muted'),
        facts([['Model ID', model.api_id], ['Storage size', model.size_bytes > 0 ? bytes(model.size_bytes) : 'Not reported'], ['Context length', model.context_length || 'Not reported'], ['Parameters', model.parameter_count?.toLocaleString() || 'Not reported']]),
        actions(leaveButton(model.is_cloud ? 'Add cloud model' : 'Download', () => deploymentForm(owner.panel.view(model.is_cloud ? 'Add cloud model' : 'Download model'), model, show), p.api, 'primary')));
      if (model.tags?.length) owner.root.append(el('details', {}, el('summary', {}, 'Tags'), text(model.tags.join(', '))));
      if (model.readme) owner.root.append(el('details', {}, el('summary', {}, 'Model card'), el('pre', { class: 'log-output' }, model.readme)));
    } catch (error) { if (owner.current()) owner.root.replaceChildren(problem(error), leaveButton('Reload model card', show, p.api)); }
  };
  await show();
}

export async function deploymentForm(p, model, back) {
  const form = el('form', { class: 'entity-editor' }), draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const provider = select('download_provider', [['', 'Choose a local provider…']], ''); provider.required = !model.is_cloud;
  const file = select('download_file', [['', 'Registry default']], '');
  const featureChoices = el('div'), nodeChoices = el('div'), assessment = el('div'), review = el('div');
  const feedback = el('div', { 'aria-live': 'polite' });
  const validation = formFeedback(form, feedback, key => ({ model: provider, provider, file, nodes: nodeChoices, features: featureChoices })[key]);
  const force = input('download_force', '', { type: 'checkbox' }), restart = input('download_restart', '', { type: 'checkbox' });
  const check = button('Review download', reviewDownload, 'primary');
  const submit = el('button', { type: 'submit', class: 'primary', hidden: true, 'data-protected-actions': true }, model.is_cloud ? 'Add cloud model' : 'Start download');
  const reads = new Lifetime(); p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  let variants = [], features = [], nodes = [], selections = new Map(), featureInputs = new Map(), reviewed, busy = false, blocked = false;
  controls.append(el('h2', {}, model.display_name || model.api_id), text(model.api_id, 'muted'));
  if (!model.is_cloud) controls.append(field('Local provider', provider), field(model.provider === 'ollama' ? 'Version tag' : 'Weights variant', file), featureChoices,
    el('section', { class: 'access-section' }, el('h3', {}, 'Target nodes'), text('Select the nodes to receive this model. Capacity observations are advisory. The coordinator validates the final request.', 'muted'), nodeChoices, assessment),
    el('details', {}, el('summary', {}, 'Advanced options'), field('Replace existing files', force), field('Restart affected running models after download', restart)));
  else controls.append(text(`Register this model with ${model.provider}. No local download is required.`, 'muted'));
  const footer = editorActions(check, () => { if (draft?.discard() !== false) back(); }, p.api);
  footer.append(submit);
  form.append(feedback, controls, review, footer);
  p.root.append(form);
  function invalidate() { reviewed = undefined; submit.hidden = true; check.hidden = false; review.replaceChildren(); }
  form.addEventListener('input', invalidate); form.addEventListener('change', invalidate);
  function body() {
    return { model: model.api_id, registry: model.provider, ...(!model.is_cloud ? { provider: provider.value, nodes: [...selections].filter(([, control]) => control.checked).map(([name]) => name), ...(file.value ? { file: file.value } : {}),
      ...(features.length ? { features: [...featureInputs].filter(([, control]) => control.checked).map(([name]) => name) } : {}), ...(force.checked ? { force: true } : {}), ...(restart.checked ? { restart: 'affected' } : {}) } : {}) };
  }
  async function loadChoices() {
    invalidate();
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    const selectedProvider = provider.value;
    file.disabled = true; check.disabled = true; featureInputs = new Map(); features = [];
    featureChoices.replaceChildren(); assessment.replaceChildren(message('Reading capacity observations…'));
    const query = new URLSearchParams({ model: model.api_id, registry: model.provider, provider: selectedProvider });
    const [selection, capacity] = await Promise.allSettled([
      model.provider === 'huggingface' ? p.api.data('/registries/huggingface/variants?' + new URLSearchParams({ id: model.api_id, provider: selectedProvider }), { signal }) :
        model.provider === 'ollama' ? p.api.data('/registries/ollama/tags?' + new URLSearchParams({ id: model.api_id }), { signal }) : Promise.resolve({}),
      p.api.request('/zzrouter/v1/nodes/compatible?' + query, { signal, timeoutMs: 60000 }),
    ]);
    if (!p.current() || !scope.current()) return;
    feedback.replaceChildren(); file.replaceChildren(el('option', { value: '' }, 'Registry default'));
    if (selection.status === 'fulfilled') {
      const value = selection.value; variants = value.variants || value.tags || []; features = value.features || [];
      file.replaceChildren(el('option', { value: '' }, variants.length ? 'Choose a variant…' : 'Complete repository'), ...variants.map(item => el('option', { value: item.file || item.name }, [item.quantization || item.name || item.file, item.size_human || item.size, item.shards > 1 ? `${item.shards} shards` : '', item.is_cloud ? 'Cloud' : ''].filter(Boolean).join(' · '))));
      file.required = variants.length > 0;
      if (features.length) featureChoices.append(el('h3', {}, 'Optional model features'), text('Feature files are downloaded alongside the selected weights.', 'muted'), ...features.map(item => {
        const control = input('feature_' + item.feature, '', { type: 'checkbox' }); featureInputs.set(item.feature, control);
        return field(`${item.feature} · ${item.size_human || bytes(item.size_bytes)}`, control, item.file);
      }));
    } else feedback.append(problem(selection.reason), leaveButton('Reload choices', loadChoices, p.api));
    file.disabled = false;
    if (capacity.status === 'fulfilled') {
      const assessments = capacity.value.metadata?.assessments || [];
      assessment.replaceChildren(...nodes.map(node => {
        const evidence = assessments.filter(item => item.node === node.name);
        return el('details', {}, el('summary', {}, `${node.name}: ${evidence.map(item => item.status).join(', ') || 'capacity unknown'}`), ...evidence.map(item => facts([['Provider', item.provider], ['Status', item.status], ['Reason', item.reason], ['Required', Number.isFinite(item.required_mib) ? `${item.required_mib} MiB` : 'Not reported'], ['Capacity', Number.isFinite(item.capacity_mib) ? `${item.capacity_mib} MiB` : 'Not reported'], ['Unknown cause', item.unknown_cause || 'None reported']])), !evidence.length ? text('No capacity evidence was returned for this node.') : null);
      }));
    } else assessment.replaceChildren(message('Capacity could not be read. Nodes remain available for explicit selection; the coordinator will validate the request.', 'warning'), problem(capacity.reason), leaveButton('Retry capacity and variants', loadChoices, p.api));
    check.disabled = selection.status !== 'fulfilled';
  }
  function reviewDownload() {
    if (busy || blocked || !p.current() || !form.reportValidity()) return;
    const value = body();
    if (!model.is_cloud && !value.nodes.length) { validation.error(invalidField('Select at least one target node.', selections.values().next().value || nodeChoices)); return; }
    reviewed = value;
    review.replaceChildren(el('section', { class: 'access-section' }, el('h3', {}, 'Review selection'), facts([['Model', value.model], ['Registry', value.registry], ['Provider', value.provider || model.provider], ['Nodes', value.nodes?.join(', ') || 'Cloud registration'], ['Variant', value.file || 'Registry default'], ['Features', value.features?.join(', ') || 'None selected'], ['Replace files', value.force ? 'Yes' : 'No'], ['Restart affected runs', value.restart ? 'Yes' : 'No']]), text('Confirm to submit this exact selection once.', 'muted')));
    check.hidden = true; submit.hidden = false;
  }
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!reviewed || busy || blocked || !p.current() || !p.api.canChange()) return;
    busy = true; blocked = true; controls.disabled = true; submit.disabled = true;
    const submitted = reviewed;
    let result, error;
    try { result = await p.api.data('/deployments', { method: 'POST', body: submitted, signal: p.scope.signal, timeoutMs: 120000, idempotencyKey: crypto.randomUUID() }); }
    catch (reason) { error = reason; }
    if (!p.current()) return;
    draft?.clean();
    const owner = p.panel.view('Download result');
    if (error || !result?.id) {
      owner.root.append(el('h2', {}, model.api_id), facts([['Provider', submitted.provider || model.provider], ['Nodes', submitted.nodes?.join(', ') || 'Cloud registration']]), problem(error || new Error('The coordinator did not return a deployment identity. Inspect Downloads before submitting again.')), leaveButton('Inspect Downloads', () => p.go('workloads', undefined, 'downloads'), p.api));
      return;
    }
    await inspectDeployment(owner, result.id, result);
  });
  provider.addEventListener('change', () => { if (!busy && !blocked) void loadChoices(); });
  if (model.is_cloud) return;
  check.disabled = true;
  try {
    const [catalog, inventory] = await Promise.all([p.api.list('/providers/catalog?kind=all', { signal: p.scope.signal }), p.api.list('/nodes', { signal: p.scope.signal })]);
    if (!p.current()) return;
    nodes = inventory;
    provider.append(...catalog.filter(item => providerCategory(item) === 'local').map(item => el('option', { value: item.key }, item.name || item.key)));
    selections = new Map();
    nodeChoices.replaceChildren(...nodes.map(node => { const control = input('node_' + node.name, '', { type: 'checkbox' }); selections.set(node.name, control); return field(node.name, control, node.health_status === 'down' ? 'Offline. A request may be refused until it reconnects.' : node.health_status || 'Health not reported'); }));
    if (!nodes.length) feedback.replaceChildren(message('No compute nodes were reported. Add a node before downloading a local model.', 'warning'));
    assessment.replaceChildren(text('Choose a provider to inspect variants and capacity.', 'muted'));
  } catch (error) { if (p.current()) validation.error(error); }
}
