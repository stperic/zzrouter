import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, actions, field, input, select, facts, message, problem, markSelection, editorActions, formFeedback, invalidField } from './dom.mjs';
import { details } from './details.mjs';
import { bytes, discoveryKey, discoveryName, discoveryIdentity, searchPath, nextSearchPage, searchSelection, discoveryRows, modelKey } from './model-data.mjs';
import { inspectDeployment } from './deployments.mjs';
import { providerCategory } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';
import { modelCount, modelList, modelRow, modelToolbar } from './model-list.mjs';
import { descriptionPreview, modelLink, modelMarkdown } from './discovery-markdown.mjs';
import { discoveryDates, providerCardInfo } from './model-catalog-details.mjs';

const sortLabels = { downloads: 'Most downloaded', popular: 'Most popular', newest: 'Recently updated',
  modified: 'Recently updated', lastModified: 'Recently updated', name: 'Name A to Z', likes: 'Most liked',
  trendingScore: 'Trending', date: 'Newest first', createdAt: 'Newest first', author: 'Author', context: 'Largest context',
  price: 'Lowest price', 'price-desc': 'Highest price' };

const downloadCountFormat = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
const discoveryTagGroups = {
  capability: [
    ['Tools', 'blue', 'tools', 'tool-use', 'function-calling'],
    ['Thinking', 'purple', 'thinking', 'reasoning'],
    ['Vision', 'green', 'vision', 'image', 'image-to-text', 'image-text-to-text', 'visual-question-answering'],
    ['Image generation', 'green', 'text-to-image', 'image-generation'],
    ['Image editing', 'green', 'image-to-image', 'image-editing', 'image-inpainting'],
    ['Video generation', 'purple', 'text-to-video', 'image-to-video', 'image-text-to-video', 'video-to-video', 'audio-to-video',
      'text-to-audio-video', 'image-to-audio-video', 'image-text-to-audio-video', 'video-generation'],
    ['Video understanding', 'purple', 'video-text-to-text', 'video-to-text', 'video-classification'],
    ['Embeddings', 'teal', 'embedding', 'embeddings', 'feature-extraction', 'sentence-similarity'],
    ['Reranking', 'teal', 'reranking', 'reranker'],
    ['Decision', 'amber', 'decision', 'classification', 'text-classification', 'zero-shot-classification'],
    ['Speech recognition', 'rose', 'automatic-speech-recognition', 'speech-to-text'],
    ['Text to speech', 'rose', 'text-to-speech'],
    ['Speech to speech', 'rose', 'speech-to-speech'],
    ['Audio generation', 'rose', 'text-to-audio', 'video-to-audio', 'audio-generation', 'text-to-music', 'music-generation'],
    ['Translation', 'blue', 'translation'],
    ['Summarization', 'blue', 'summarization'],
    ['Coding', 'blue', 'code', 'coding', 'code-generation'],
  ],
  general: [
    ['Chat', 'blue', 'chat', 'conversational'],
    ['Text generation', 'blue', 'text-generation', 'text2text-generation'],
    ['Audio', 'rose', 'audio', 'speech', 'audio-to-audio', 'audio-classification'],
  ],
  library: [
    ['Transformers', '', 'transformers'],
    ['Diffusers', '', 'diffusers'],
    ['ComfyUI', '', 'comfyui'],
    ['Sentence Transformers', '', 'sentence-transformers'],
    ['llama.cpp', '', 'llama.cpp', 'llamacpp'],
  ],
  format: [
    ['Safetensors', '', 'safetensors', 'safe-tensors'],
    ['GGUF', '', 'gguf'],
    ['MLX', '', 'mlx', 'mlx-lm', 'mlx-vlm'],
    ['ONNX', '', 'onnx'],
  ],
};
const discoveryTagIndex = new Map(Object.entries(discoveryTagGroups).flatMap(([kind, entries]) =>
  entries.flatMap(([label, tone, ...aliases]) => {
    const tag = { kind, label, tone };
    return aliases.map(alias => [alias, tag]);
  })));

function uniqueDiscoveryTags(tags) {
  const labels = new Map();
  for (const tag of tags) {
    if (typeof tag !== 'string') continue;
    const label = tag.trim();
    const key = label.toLowerCase();
    if (key && !labels.has(key)) labels.set(key, label);
  }
  return [...labels.values()];
}

function discoveryPills(tags) {
  if (!tags.length) return null;
  return el('span', { class: 'model-tags' },
    tags.map(({ label, tone }) => el('span', { class: 'badge', 'data-tone': tone || undefined }, label)));
}

function primaryDiscoveryTags(tags, task, library) {
  const present = [...new Set(uniqueDiscoveryTags([task, library, ...tags])
    .map(label => discoveryTagIndex.get(label.toLowerCase())).filter(Boolean))];
  const capabilities = present.filter(tag => tag.kind === 'capability');
  return discoveryPills([
    ...(capabilities.length ? capabilities.slice(0, 3) : present.filter(tag => tag.kind === 'general').slice(0, 1)),
    ...present.filter(tag => tag.kind === 'library').slice(0, 1),
    ...present.filter(tag => tag.kind === 'format').slice(0, 2),
  ]);
}

function discoveryDate(item) {
  const date = discoveryDates(item)[0];
  return date ? el('span', { class: 'network-meta' }, date[0] + ' ', date[1]) : null;
}

function discoveryStat(icon, label, fullLabel) {
  return el('span', { class: 'discovery-stat', title: fullLabel },
    el('span', { class: `discovery-stat-icon discovery-stat-${icon}`, 'aria-hidden': 'true' }),
    fullLabel ? el('span', { 'aria-hidden': 'true' }, label) : label,
    fullLabel ? el('span', { class: 'visually-hidden' }, fullLabel) : null);
}

export async function discoverModels(p, selected) {
  const query = input('discovery_query', '', { type: 'search', placeholder: 'Search model names' });
  const provider = select('discovery_provider', [], '');
  const sort = select('discovery_sort', [['', 'Provider default']], '');
  const tags = input('discovery_tags', '', { placeholder: 'Comma-separated tags' });
  const logic = select('discovery_tag_logic', [['AND', 'Match all tags'], ['OR', 'Match any tag']], 'AND');
  const author = input('discovery_author');
  const sourceInfo = el('p', { class: 'muted' });
  const filters = el('details', {}, el('summary', {}, 'More filters'), field('Tags', tags), field('Tag matching', logic), field('Author', author));
  const submitSearch = el('button', { type: 'submit', class: 'primary' }, 'Search');
  const form = el('form', { class: 'discovery-filters' },
    modelToolbar(field('Registry or cloud provider', provider), field('Search', query), field('Sort', sort), submitSearch),
    sourceInfo, filters);
  const { workspace, feedback, results, pagination } = modelList(form);
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
    const ollama = source?.name === 'ollama';
    const sortOptions = ollama ? ['downloads', 'modified', 'name'] : new Set([...(source?.sort_options || []), ...(source?.client_sorts || [])]);
    sort.replaceChildren(el('option', { value: '' }, 'Provider default'), ...[...sortOptions].map(value => {
      const label = source.is_cloud && value === 'newest' ? 'Newest first' : sortLabels[value] || value;
      const suffix = searchSelection(source, { sort: value }).viewSort ? ' (this page)' : '';
      return el('option', { value }, label + suffix);
    }));
    tags.disabled = logic.disabled = author.disabled = !['huggingface', 'all'].includes(source?.name);
    filters.hidden = tags.disabled;
    query.disabled = sort.disabled = submitSearch.disabled = source?.discovery_available === false;
    sourceInfo.textContent = !ollama && !source?.is_cloud && source?.client_filter ? 'Search filters the current provider page.' : '';
    sourceInfo.hidden = !sourceInfo.textContent;
  };
  const configureProvider = source => leaveButton('Configure provider', () => p.go('providers', `cloud/${source.name}`), p.api);
  const refreshProviders = () => leaveButton('Refresh providers', () => loadSources(provider.value), p.api);
  async function read(page = {}, isNew = false) {
    if (!p.current() || !(p.canLeave?.() ?? p.api.canLeave())) return;
    const source = sources.find(item => item.name === provider.value);
    if (!source) { feedback.replaceChildren(message('Choose a model discovery provider first.')); return; }
    if (isNew) {
      activeQuery = searchSelection(source, { query: query.value.trim(), sort: sort.value, tags: tags.disabled ? '' : tags.value.trim(), logic: logic.value, author: author.disabled ? '' : author.value.trim() });
      history = [{}]; pageIndex = 0;
    }
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    pagination.replaceChildren();
    records = []; results.replaceChildren();
    if (source.discovery_available === false) {
      const unsupported = source.discovery_supported === false;
      feedback.replaceChildren(
        message(unsupported ? 'Model discovery is unavailable through this connection.' :
          'Add the provider’s credentials, such as an API key, to browse its models.', 'warning'),
        actions(!unsupported && configureProvider(source), refreshProviders()));
      return;
    }
    feedback.replaceChildren(message('Searching models…'));
    try {
      const result = await p.api.data(searchPath({ ...activeQuery, ...page }), { signal, timeoutMs: 60000 });
      if (!p.current() || !scope.current()) return;
      records = result.results || [];
      const visible = discoveryRows(records, activeQuery);
      const nameCounts = new Map();
      for (const { discovery } of visible) {
        const name = discoveryName(discovery);
        nameCounts.set(name, (nameCounts.get(name) || 0) + 1);
      }
      results.replaceChildren(...visible.map(({ discovery: item, pipeline_tag: task, library_name: library }) => {
        const key = discoveryKey(item);
        const name = discoveryName(item);
        const ollama = item.provider === 'ollama';
        const tags = uniqueDiscoveryTags(item.tags || []);
        const tagCount = ollama ? item.variant_count : tags.length;
        const metadata = [
          item.downloads > 0 ? discoveryStat('download', downloadCountFormat.format(item.downloads),
            `${item.downloads.toLocaleString()} ${item.downloads === 1 ? 'download' : 'downloads'}`) : null,
          tagCount > 0 ? discoveryStat('tag', `${tagCount.toLocaleString()} ${tagCount === 1 ? 'tag' : 'tags'}`) : null,
          !ollama && item.size_bytes > 0 ? el('span', {}, bytes(item.size_bytes)) : null,
          item.context_length ? el('span', {}, `${item.context_length.toLocaleString()} token context`) : null,
          ...(item.parameter_sizes || []).map(size => el('span', { class: 'badge discovery-parameter' }, size)),
        ].filter(Boolean);
        return modelRow(p.api, { key, name, open: () => p.go('models', key, 'discover'), date: discoveryDate(item),
          tags: primaryDiscoveryTags(tags, task, library),
          source: nameCounts.get(name) > 1 && item.api_id !== name ? item.api_id : '',
          description: item.description !== name && item.description !== item.api_id ? descriptionPreview(item.description) : '', stats: metadata });
      }));
      markSelection(results, selected);
      const next = nextSearchPage(result);
      const count = modelCount(visible.length);
      const total = result.total > visible.length ? ` · ${result.is_total_exact ? '' : 'about '}${result.total.toLocaleString()} total` : '';
      feedback.replaceChildren(text(visible.length ? count + total : next ? 'No matches on this page. Try the next page.' : 'No models match this search.', 'muted'));
      if (pageIndex > 0) pagination.append(leaveButton('Previous', () => { pageIndex--; return read(history[pageIndex]); }, p.api));
      if (next) pagination.append(leaveButton('Next', () => { history[++pageIndex] = next; return read(next); }, p.api));
    } catch (error) {
      if (!p.current() || !scope.current()) return;
      const retry = leaveButton('Retry search', () => read(page), p.api);
      if (source.is_cloud && error.code === 'provider_connection_unavailable') {
        feedback.replaceChildren(problem(error), actions(configureProvider(source), refreshProviders()));
      } else {
        feedback.replaceChildren(problem(error), retry);
      }
    }
  }
  provider.addEventListener('change', () => { updateFilters(); void read({}, true); });
  sort.addEventListener('change', () => { void read({}, true); });
  form.addEventListener('submit', event => { event.preventDefault(); void read({}, true); });
  async function loadSources(preferred) {
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
      const previousSort = provider.value === preferred ? sort.value : '';
      provider.replaceChildren(...[available, setup, unsupported].filter(group => group.childElementCount));
      if (sources.some(item => item.name === preferred)) provider.value = preferred;
      updateFilters();
      if ([...sort.children].some(option => option.value === previousSort)) sort.value = previousSort;
      p.handleSelection?.(choose, (_key, mode) => mode === 'discover');
      if (provider.value) await read({}, true);
      else feedback.replaceChildren(message('The coordinator did not declare any model discovery providers.'), refreshProviders());
    } catch (error) {
      if (p.current() && scope.current()) feedback.replaceChildren(problem(error), refreshProviders());
    }
  }
  p.root.append(workspace);
  await loadSources(discoveryIdentity(selected).provider);
  if (selected && sources.length && p.current()) await choose(selected);
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
      const tags = uniqueDiscoveryTags([...(model.tags || []), ...(initial?.discovery?.tags || [])]);
      const info = providerCardInfo(model, native);
      const description = discoveryDescription(owner, cardPath, model, info.base);
      const allTags = tags.length ? el('details', { class: 'discovery-all-tags' }, el('summary', {}, 'All tags'),
        discoveryPills(tags.map(label => ({ label, tone: discoveryTagIndex.get(label.toLowerCase())?.tone })))) : null;
      owner.root.replaceChildren(...[
        el('h2', {}, discoveryName(model)), providerName(model.provider, model.provider), description,
        primaryDiscoveryTags(tags, native.pipeline_tag, native.library_name), info.facts,
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
  const ollama = model.provider === 'ollama';
  const form = el('form', { class: 'entity-editor' }), draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const provider = select('download_provider', [['', 'Choose a local provider…']], ''); provider.required = !model.is_cloud;
  const file = select('download_file', [['', ollama ? 'Loading versions…' : 'Registry default']], '');
  const versionInfo = el('p', { class: 'muted', 'aria-live': 'polite', hidden: true });
  const featureChoices = el('div'), nodeChoices = el('div'), assessment = el('div'), review = el('div');
  const feedback = el('div', { 'aria-live': 'polite' });
  const validation = formFeedback(form, feedback, key => ({ model: provider, provider, file, nodes: nodeChoices, features: featureChoices })[key]);
  const force = input('download_force', '', { type: 'checkbox' }), restart = input('download_restart', '', { type: 'checkbox' });
  const check = button('Review download', reviewDownload, 'primary');
  const submit = el('button', { type: 'submit', class: 'primary', hidden: !model.is_cloud, 'data-protected-actions': true }, model.is_cloud ? 'Add cloud model' : 'Start download');
  const edit = button('Edit selection', () => { invalidate(); file.focus(); });
  edit.hidden = true;
  const reads = new Lifetime(); p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  let variants = [], features = [], selections = new Map(), featureInputs = new Map(), reviewed, busy = false, blocked = false;
  let downloadReady = false, ggufArchitecture = '';
  provider.disabled = file.disabled = check.disabled = !model.is_cloud;
  controls.append(el('h2', {}, discoveryName(model)));
  if (model.display_name && model.display_name !== model.api_id) controls.append(text(model.api_id, 'muted'));
  if (!model.is_cloud) controls.append(field('Local provider', provider), field(ollama ? 'Version tag' : 'Weights variant', file), ...(ollama ? [versionInfo] : []), featureChoices,
    el('section', { class: 'access-section' }, el('h3', {}, 'Target nodes'), nodeChoices, assessment),
    el('details', {}, el('summary', {}, 'Advanced options'), field('Replace existing files', force), field('Restart affected running models after download', restart)));
  else controls.append(text(`Register this model with ${model.provider}. No local download is required.`, 'muted'));
  const footer = editorActions(model.is_cloud ? submit : check, back, p.api, draft);
  if (!model.is_cloud) footer.prepend(submit, edit);
  form.append(feedback, controls, review, footer);
  p.root.append(form);
  function invalidate() {
    reviewed = undefined;
    controls.hidden = check.hidden = false;
    submit.hidden = !model.is_cloud;
    edit.hidden = true;
    review.replaceChildren();
  }
  form.addEventListener('input', invalidate); form.addEventListener('change', invalidate);
  const selectedVariant = () => variants.find(item => (item.file || item.name) === file.value);
  const selectedFormat = () => {
    if (selectedVariant()?.is_mlx) return 'mlx';
    return /\.gguf$/i.test(file.value) ? 'gguf' : '';
  };
  function body() {
    return { model: model.api_id, registry: model.provider, ...(!model.is_cloud ? { provider: provider.value, nodes: [...selections].filter(([, control]) => control.checked).map(([name]) => name), ...(file.value ? { file: file.value } : {}),
      ...(selectedFormat() ? { format: selectedFormat() } : {}),
      ...(features.length ? { features: [...featureInputs].filter(([, control]) => control.checked).map(([name]) => name) } : {}), ...(force.checked ? { force: true } : {}), ...(restart.checked ? { restart: 'affected' } : {}) } : {}) };
  }
  function showNodes(targets, memory) {
    const previous = selections;
    selections = new Map();
    nodeChoices.replaceChildren(...targets.map(node => {
      const control = input('node_' + node.name, '', { type: 'checkbox', checked: previous.get(node.name)?.checked ?? Boolean(p.computeNode) });
      selections.set(node.name, control);
      const status = memory.get(node.name)?.status === 'unknown' ? 'Memory fit has not been measured.' : '';
      return field(node.name, control, status);
    }));
  }
  async function loadTargets() {
    const variant = selectedVariant();
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    downloadReady = false;
    check.disabled = true;
    versionInfo.textContent = [variant?.is_mlx ? 'MLX' : '', variant?.input, variant?.context && variant.context !== '-' ? `${variant.context} context` : ''].filter(Boolean).join(' · ');
    versionInfo.hidden = !versionInfo.textContent;
    nodeChoices.replaceChildren();
    if (file.required && !variant) {
      assessment.replaceChildren(text(ollama ? 'Choose a version to see eligible nodes.' : 'Choose a variant to see eligible nodes.', 'muted'));
      return;
    }
    assessment.replaceChildren(message(ollama ? 'Checking nodes for this version…' : 'Checking nodes for this download…'));
    const query = new URLSearchParams({ model: ollama ? `${model.api_id}:${variant.name}` : model.api_id, registry: model.provider, provider: provider.value });
    const format = selectedFormat();
    if (format) query.set('format', format);
    if (format === 'gguf' && ggufArchitecture) query.set('architecture', ggufArchitecture);
    try {
      const [inventory, capacity] = await Promise.all([
        p.api.list('/nodes', { signal }),
        p.api.request('/zzrouter/v1/nodes/compatible?' + query, { signal, timeoutMs: 60000 }),
      ]);
      if (!p.current() || !scope.current()) return;
      const unavailable = ['provider_not_eligible', 'node_unreachable', 'node_outdated', 'assessment_failed'];
      const eligible = new Map((capacity.metadata?.assessments || []).filter(item => item.provider === provider.value &&
        (item.status === 'fit' || item.status === 'unknown' && !unavailable.includes(item.unknown_cause))).map(item => [item.node, item]));
      const targets = inventory.filter(node => (!p.computeNode || p.computeNode === node.name) && node.health_status === 'healthy' &&
        eligible.has(node.name) && node.providers?.some(item => item.key === provider.value &&
          (ollama ? item.running === true : item.state === 'live' && item.run_scope !== 'node')));
      showNodes(targets, eligible);
      assessment.replaceChildren();
      if (!targets.length) assessment.append(message(ollama ?
        (p.computeNode ? `No running Ollama provider on ${p.computeNode} was confirmed eligible for this version.` :
          'No running Ollama provider on a healthy node was confirmed eligible for this version.') :
        'No healthy node was confirmed eligible for this provider and model.', 'warning'), leaveButton('Retry eligibility', loadTargets, p.api));
      downloadReady = targets.length > 0;
      check.disabled = !downloadReady;
    } catch (error) {
      if (p.current() && scope.current()) assessment.replaceChildren(problem(error), leaveButton('Retry eligibility', loadTargets, p.api));
    }
  }
  async function loadChoices() {
    invalidate();
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    const selectedProvider = provider.value;
    const selectedFile = file.value;
    downloadReady = false;
    file.disabled = true; check.disabled = true; featureInputs = new Map(); features = [];
    feedback.replaceChildren(); featureChoices.replaceChildren(); nodeChoices.replaceChildren();
    assessment.replaceChildren(message(ollama ? 'Reading versions…' : 'Reading model files…'));
    if (!ollama && !selectedProvider) {
      file.replaceChildren(el('option', { value: '' }, 'Choose a provider first'));
      assessment.replaceChildren();
      return;
    }
    const path = model.provider === 'huggingface' ? '/registries/huggingface/variants?' + new URLSearchParams({ id: model.api_id, provider: selectedProvider }) :
      ollama ? '/registries/ollama/tags?' + new URLSearchParams({ id: model.api_id }) : '';
    let value;
    try { value = path ? await p.api.data(path, { signal }) : {}; }
    catch (error) {
      if (!p.current() || !scope.current()) return;
      file.replaceChildren(el('option', { value: '' }, 'Versions unavailable'));
      assessment.replaceChildren();
      feedback.append(problem(error), leaveButton('Retry download options', loadChoices, p.api));
      return;
    }
    if (!p.current() || !scope.current()) return;
    const available = value.variants || value.tags || [];
    variants = ollama ? available.filter(item => !item.is_cloud) : available;
    features = value.features || [];
    ggufArchitecture = value.architecture || '';
    const placeholder = ollama ? (variants.length ? 'Choose a version…' : 'No local versions') : (variants.length ? 'Choose a variant…' : 'Complete repository');
    file.replaceChildren(el('option', { value: '' }, placeholder), ...variants.map(item => {
      const size = item.size_human || item.size;
      return el('option', { value: item.file || item.name },
        [item.quantization || item.name || item.file, item.is_mlx ? 'MLX' : '', size === '-' ? '' : size, item.input,
          item.shards > 1 ? `${item.shards} shards` : ''].filter(Boolean).join(' · '));
    }));
    file.required = ollama || variants.length > 0;
    if (variants.some(item => (item.file || item.name) === selectedFile)) file.value = selectedFile;
    if (ollama && !variants.length) feedback.append(message('The registry returned no downloadable versions for this model.', 'warning'));
    if (ollama && available.some(item => item.is_cloud)) featureChoices.append(text('Hosted versions are listed under Ollama Cloud in Discover.', 'muted'));
    if (features.length) featureChoices.append(el('h3', {}, 'Optional model features'), text('Feature files are downloaded alongside the selected weights.', 'muted'), ...features.map(item => {
      const control = input('feature_' + item.feature, '', { type: 'checkbox' }); featureInputs.set(item.feature, control);
      return field(`${item.feature} · ${item.size_human || bytes(item.size_bytes)}`, control, item.file);
    }));
    file.disabled = ollama && !variants.length;
    assessment.replaceChildren();
    if (!file.disabled) await loadTargets();
  }
  function reviewDownload() {
    if (!downloadReady || busy || blocked || !p.current() || !form.reportValidity()) return;
    const value = body();
    if (!model.is_cloud && !value.nodes.length) { validation.error(invalidField('Select at least one target node.', selections.values().next().value || nodeChoices)); return; }
    reviewed = value;
    const heading = el('h2', { tabindex: '-1' }, 'Review download');
    const variant = selectedVariant();
    const size = variant?.size_human || variant?.size;
    feedback.replaceChildren();
    controls.hidden = check.hidden = true;
    review.replaceChildren(heading, facts([['Model', value.model], ['Registry', value.registry], ['Provider', value.provider],
      ['Target nodes', value.nodes.join(', ')], [ollama ? 'Version tag' : 'Weights variant', value.file || 'Registry default'],
      ...(variant?.is_mlx ? [['Format', 'MLX']] : []), ...(variant?.input ? [['Input', variant.input]] : []),
      ...(size && size !== '-' ? [['Storage per node', size]] : []), ...(value.features?.length ? [['Optional features', value.features.join(', ')]] : []),
      ...(value.force ? [['Replace files', 'Yes']] : []), ...(value.restart ? [['Restart affected runs', 'Yes']] : [])]));
    submit.hidden = edit.hidden = false;
    heading.focus();
  }
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if ((!model.is_cloud && !reviewed) || busy || blocked || !p.current() || !p.api.canChange() || !form.reportValidity()) return;
    busy = true; blocked = true; controls.disabled = true; submit.disabled = true;
    const submitted = model.is_cloud ? body() : reviewed;
    let result, error;
    try { result = await p.api.data('/deployments', { method: 'POST', body: submitted, signal: p.scope.signal, timeoutMs: 120000, idempotencyKey: crypto.randomUUID() }); }
    catch (reason) { error = reason; }
    if (!p.current()) return;
    draft?.clean();
    const owner = p.panel.view(model.is_cloud ? 'Add cloud model' : 'Download result');
    if (model.is_cloud) {
      const completed = !error && result?.id && result.status === 'completed';
      if (completed) {
        owner.root.append(message('Model added. Reading its current details…'));
        try {
          const node = result.nodes?.length === 1 ? result.nodes[0].node : undefined;
          if (!node) throw new Error('The coordinator did not report the registration owner.');
          const query = new URLSearchParams({ model: submitted.model, provider: submitted.registry, registry: submitted.registry, node });
          const records = await p.api.list('/models?' + query, { signal: owner.scope.signal });
          if (!owner.current()) return;
          const matches = records.filter(item => item.is_cloud && item.name === submitted.model && item.assigned_app === submitted.registry && item.source_repo === submitted.registry && item.node === node);
          if (matches.length !== 1) throw new Error('The exact cloud model registration was not returned in Available models.');
          owner.go('models', modelKey(matches[0]), 'available', '', '');
          return;
        } catch (reason) { error = reason; }
      }
      if (!owner.current()) return;
      owner.root.replaceChildren(el('h2', {}, model.api_id), facts([['Provider', submitted.registry], ['Registration status', result?.status || 'Not confirmed']]),
        message(completed ? 'The model was added, but its current details could not be loaded.' : result?.message || 'The cloud model registration was not confirmed.', 'warning'),
        ...(error ? [problem(error)] : []), ...(result?.nodes || []).filter(item => item.error).map(item => message(item.error, 'error')),
        text('Inspect Available models before adding this model again. Registration is never replayed automatically.', 'muted'),
        leaveButton('Inspect Available models', () => owner.go('models', undefined, 'available', '', ''), p.api));
      return;
    }
    if (error || !result?.id) {
      owner.root.append(el('h2', {}, model.api_id), facts([['Provider', submitted.provider || model.provider], ['Nodes', submitted.nodes?.join(', ') || 'Cloud registration']]), problem(error || new Error('The coordinator did not return a deployment identity. Inspect Downloads before submitting again.')), leaveButton('Inspect Downloads', () => p.go('workloads', undefined, 'downloads'), p.api));
      return;
    }
    await inspectDeployment(owner, result.id, result);
  });
  file.addEventListener('change', () => { if (!busy && !blocked) void loadTargets(); });
  if (!ollama) provider.addEventListener('change', () => { if (!busy && !blocked) void loadChoices(); });
  if (model.is_cloud) return;
  if (ollama) {
    provider.replaceChildren(el('option', { value: 'ollama' }, 'Ollama'));
    provider.value = 'ollama';
    await loadChoices();
    return;
  }
  async function initialize() {
    feedback.replaceChildren(message('Reading download options…'));
    try {
      const options = { signal: p.scope.signal };
      const [catalog, inventory] = await Promise.all([p.api.list('/providers/catalog?kind=all', options), p.api.list('/nodes', options)]);
      if (!p.current()) return;
      const nodes = p.computeNode ? inventory.filter(node => node.name === p.computeNode) : inventory;
      if (!nodes.length) {
        const reason = p.computeNode ? `${p.computeNode} is not in the current inventory. Reconnect it before downloading a model.` :
          'No compute nodes were reported. Add a node before downloading a local model.';
        feedback.replaceChildren(message(reason, 'warning'), leaveButton('Refresh nodes', initialize, p.api));
        return;
      }
      const running = new Set(nodes.filter(node => node.health_status === 'healthy').flatMap(node =>
        (node.providers || []).filter(item => item.state === 'live' && item.run_scope !== 'node').map(item => item.key)));
      const local = catalog.filter(item => providerCategory(item) === 'local' && item.protocol !== 'ollama' && running.has(item.key));
      provider.disabled = !local.length;
      provider.replaceChildren(el('option', { value: '' }, 'Choose a local provider…'), ...local.map(item => el('option', { value: item.key }, item.name || item.key)));
      feedback.replaceChildren();
      if (!local.length) feedback.append(message('No compatible local provider is enabled on a healthy node. Check Providers to set up a runtime.', 'warning'), leaveButton('Refresh providers', initialize, p.api));
      else if (local.some(item => item.key === p.computeProvider)) { provider.value = p.computeProvider; await loadChoices(); }
    } catch (error) {
      if (p.current()) feedback.replaceChildren(problem(error), leaveButton('Retry download options', initialize, p.api));
    }
  }
  await initialize();
}
