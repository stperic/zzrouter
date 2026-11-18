import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, actions, field, input, select, facts, message, problem, badge, markSelection, editorActions, formFeedback, invalidField } from './dom.mjs';
import { details } from './details.mjs';
import { bytes, discoveryKey, discoveryIdentity, searchPath, nextSearchPage, searchSelection, discoveryRows } from './model-data.mjs';
import { inspectDeployment } from './deployments.mjs';
import { providerCategory } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';
import { descriptionPreview, modelLink, modelMarkdown } from './discovery-markdown.mjs';
import { providerCardInfo } from './model-catalog-details.mjs';

export async function discoverModels(p, selected) {
  const query = input('discovery_query', '', { type: 'search', placeholder: 'Search model names' });
  const provider = select('discovery_provider', [], '');
  const sort = select('discovery_sort', [['', 'Provider default']], '');
  const tags = input('discovery_tags', '', { placeholder: 'Comma-separated tags' });
  const logic = select('discovery_tag_logic', [['AND', 'Match all tags'], ['OR', 'Match any tag']], 'AND');
  const author = input('discovery_author');
  const sourceInfo = el('p', { class: 'muted' });
  const submitSearch = el('button', { type: 'submit', class: 'primary' }, 'Search');
  const filters = el('details', {}, el('summary', {}, 'More filters'), field('Tags', tags), field('Tag matching', logic), field('Author', author));
  const feedback = el('div', { 'aria-live': 'polite' }), results = el('div', { class: 'node-grid' }), pagination = el('div', { class: 'actions' });
  const form = el('form', {}, el('div', { class: 'workload-filters' }, field('Registry or cloud provider', provider), field('Search', query), field('Sort', sort)), sourceInfo, filters, submitSearch);
  const list = el('section', { class: 'card entity-list' }, form, feedback, results, pagination), workspace = el('div', { class: 'entity-workspace' }, list);
  const reads = new Lifetime(); p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  let sources = [], records = [], drawer, activeQuery, history = [], pageIndex = 0;
  const choose = async key => {
    drawer?.dispose(!key); drawer = undefined; selected = key; markSelection(results, selected);
    if (!key) return;
    const identity = discoveryIdentity(key);
    const source = sources.find(item => item.name === identity.provider);
    if (source && provider.value !== source.name) {
      provider.value = source.name;
      updateFilters();
      void read({}, true);
    }
    if (source?.discovery_available === false) return;
    drawer = details({ ...p, root: workspace }, 'Discover model', () => p.go('models', undefined, 'discover'));
    await discoveryDetails(drawer, identity, records.find(item => discoveryKey(item.discovery) === key));
  };
  const updateFilters = () => {
    const source = sources.find(item => item.name === provider.value);
    sort.replaceChildren(el('option', { value: '' }, 'Provider default'), ...[...new Set([...(source?.sort_options || []), ...(source?.client_sorts || [])])].map(value => el('option', { value }, `${value}${source.client_sorts?.includes(value) ? ' (this page)' : ''}`)));
    tags.disabled = !source?.has_tags; logic.disabled = !source?.has_tags;
    query.disabled = sort.disabled = submitSearch.disabled = source?.discovery_available === false;
    sourceInfo.textContent = [source?.description, source?.client_filter ? 'Name filtering applies to the current provider page. Browse more pages to see additional matches.' : '', source?.is_cloud && !source.configured ? 'This cloud connection is not configured yet.' : ''].filter(Boolean).join(' ');
  };
  async function read(page = {}, isNew = false) {
    if (!p.current() || !(p.canLeave?.() ?? p.api.canLeave())) return;
    const source = sources.find(item => item.name === provider.value);
    if (!source) { feedback.replaceChildren(message('Choose a model discovery provider first.')); return; }
    if (isNew) { activeQuery = searchSelection(source, { query: query.value.trim(), sort: sort.value, tags: tags.disabled ? '' : tags.value.trim(), logic: logic.value, author: author.value.trim() }); history = [{}]; pageIndex = 0; }
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    if (source.discovery_available === false) {
      records = [];
      results.replaceChildren();
      pagination.replaceChildren();
      const unsupported = source.discovery_supported === false;
      feedback.replaceChildren(
        message(unsupported ? 'Model discovery is unavailable through this connection.' : 'Add the provider’s credentials to browse its models.', 'warning'),
        actions(!unsupported && leaveButton('Configure provider', () => p.go('providers', `cloud/${source.name}`), p.api),
          leaveButton('Refresh providers', () => loadSources(provider.value), p.api)));
      return;
    }
    feedback.replaceChildren(message('Searching models…')); pagination.replaceChildren();
    try {
      const result = await p.api.data(searchPath({ ...activeQuery, ...page }), { signal, timeoutMs: 60000 });
      if (!p.current() || !scope.current()) return;
      records = result.results || [];
      const visible = discoveryRows(records, activeQuery);
      results.replaceChildren(...visible.map(({ discovery: item }) => {
        const key = discoveryKey(item), control = leaveButton('', () => p.go('models', key, 'discover'), p.api, 'network-item');
        const name = item.display_name || item.api_id;
        const preview = descriptionPreview(item.description);
        control.setAttribute('data-selection', key);
        control.append(el('span', { class: 'network-item-title' }, name, badge(item.is_cloud ? 'Cloud' : item.provider)));
        if (preview && preview !== name && preview !== item.api_id) control.append(text(preview, 'muted discovery-row-preview'));
        control.append(text([item.size_bytes > 0 ? bytes(item.size_bytes) : '', item.downloads ? `${item.downloads.toLocaleString()} downloads` : '', item.context_length ? `${item.context_length.toLocaleString()} context` : ''].filter(Boolean).join(' · '), 'muted'));
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
  p.root.append(workspace);
  async function loadSources(initialProvider) {
    const scope = reads.next();
    const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    feedback.replaceChildren(message('Checking provider availability…'));
    try {
      const catalog = await p.api.data('/search?type=providers', { signal });
      if (!p.current() || !scope.current()) return;
      sources = catalog.results || [];
      const available = el('optgroup', { label: 'Available' });
      const setup = el('optgroup', { label: 'Setup required' });
      const unsupported = el('optgroup', { label: 'Catalog unavailable' });
      for (const item of sources) {
        const group = item.discovery_supported === false ? unsupported : item.discovery_available === false ? setup : available;
        group.append(el('option', { value: item.name }, item.display_name || item.name));
      }
      provider.replaceChildren(...[available, setup, unsupported].filter(group => group.childElementCount));
      if (sources.some(item => item.name === initialProvider)) provider.value = initialProvider;
      updateFilters();
      if (provider.value) await read({}, true);
      else feedback.replaceChildren(message('The coordinator did not declare any model discovery providers.'));
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error)); }
  }
  p.handleSelection?.(choose, (_key, mode) => mode === 'discover');
  await loadSources(discoveryIdentity(selected).provider);
  if (selected && p.current()) await choose(selected);
}

export async function discoveryDetails(p, identity, initial) {
  const cardPath = `/models/card/${encodeURIComponent(identity.provider)}/${identity.api_id.split('/').map(encodeURIComponent).join('/')}`;
  const show = async () => {
    const owner = p.panel.view('Discover model');
    owner.root.append(message('Loading model card…'));
    try {
      const value = await p.api.data(cardPath, { signal: owner.scope.signal });
      if (!owner.current()) return;
      if (!value.discovery && !initial?.discovery) throw new Error('The coordinator returned no discovery metadata for this model.');
      const model = { ...initial?.discovery, ...value.discovery };
      const native = Object.fromEntries([...Object.entries(initial || {}), ...Object.entries(value)]
        .filter(([, item]) => item !== null && item !== undefined && item !== ''));
      const tags = [...new Set([...(model.tags || []), ...(initial?.discovery?.tags || [])])];
      const info = providerCardInfo(model, native);
      const description = discoveryDescription(owner, cardPath, model, info.base);
      const allTags = tags.length ? el('details', { class: 'discovery-all-tags' }, el('summary', {}, 'All tags'),
        el('div', { class: 'actions' }, tags.map(label => badge(label)))) : null;
      owner.root.replaceChildren(...[
        el('h2', {}, model.display_name || model.api_id), providerName(model.provider, model.provider), description,
        info.facts,
        actions(leaveButton(model.is_cloud ? 'Add cloud model' : 'Download', () => deploymentForm(owner.panel.view(model.is_cloud ? 'Add cloud model' : 'Download model'), model, show), p.api, 'primary')),
        info.links, allTags, ...info.extras,
      ].filter(Boolean));
    } catch (error) { if (owner.current()) owner.root.replaceChildren(problem(error), leaveButton('Reload model card', show, p.api)); }
  };
  await show();
}

function discoveryDescription(p, path, model, base) {
  const excerpt = model.description;
  if (typeof excerpt !== 'string' || !excerpt.trim() || excerpt === model.api_id || excerpt === model.display_name) return null;
  const preview = modelMarkdown(excerpt, base);
  if (model.provider !== 'openrouter' || !base || !/(?:\.{3}|…)\s*$/.test(excerpt)) return preview;

  const body = el('div', { id: 'discover-model-description' }, preview);
  const status = el('span', { role: 'status', class: 'muted' });
  const toggle = button('Read full description', read);
  toggle.setAttribute('aria-controls', body.id);
  toggle.setAttribute('aria-expanded', 'false');
  let full, loading = false;
  function display(expanded) {
    body.replaceChildren(expanded ? full : preview);
    toggle.textContent = expanded ? 'Show less' : 'Read full description';
    toggle.setAttribute('aria-expanded', String(expanded));
  }
  async function read() {
    if (loading) return;
    if (full) {
      display(toggle.getAttribute('aria-expanded') === 'false');
      return;
    }
    loading = true;
    toggle.setAttribute('aria-disabled', 'true');
    status.textContent = 'Loading full description…';
    try {
      const data = await p.api.data(path + '?full_description=true', { signal: p.scope.signal });
      const description = data.discovery?.description;
      if (p.current() && typeof description === 'string' && description.length > excerpt.length) {
        full = modelMarkdown(description, base);
        display(true);
        return;
      }
    } catch {
      // The excerpt and public page are still available if the request fails.
    } finally {
      loading = false;
      status.textContent = '';
      toggle.removeAttribute('aria-disabled');
    }
    if (p.current()) {
      const link = modelLink('Read on OpenRouter', base);
      const focused = document.activeElement === toggle;
      toggle.replaceWith(link);
      if (focused) link.focus();
    }
  }
  return el('div', {}, body, el('div', { class: 'actions discovery-links' }, toggle, status));
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
  const footer = editorActions(check, back, p.api, draft);
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
