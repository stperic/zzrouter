import { el, text, button, leaveButton, heading, field, input, select, message, problem, editorActions, formFeedback } from './dom.mjs';
import { details } from './details.mjs';

export const defaultSettings = () => ({ model: '', system: '', node: '', showReasoning: true, showStats: true });

export function chatModels(result) {
  if (!Array.isArray(result?.data)) throw new Error('The coordinator returned an invalid model list.');
  return result.data.filter(model => typeof model.id === 'string' && model.capabilities?.chat !== false)
    .sort((a, b) => a.id.localeCompare(b.id));
}

// One mounted runtime belongs to one browser authority, never a page or key ID.
// Detaching its canvas preserves the SDK's message, composer and cancellation state.
export function createChatSession(api, mount = () => import('./vendor/chat.mjs')) {
  const signal = api.authority.signal;
  const session = { settings: defaultSettings(), canvas: el('div', { class: 'chat-canvas' }), view: undefined };
  session.ready = mount().then(({ mountChat }) => {
    if (signal.aborted) return;
    session.view = mountChat(session.canvas, { baseURL: new URL('/v1', api.origin).href,
      fetcher: api.inferenceFetch.bind(api), settings: session.settings, signal, enabled: api.canChange() });
    return session.view;
  });
  signal.addEventListener('abort', () => { session.settings = defaultSettings(); session.view = undefined; }, { once: true });
  return session;
}

export async function chat(p, session, initialModel, initialNode) {
  let models = [];
  let drawer;
  let readVersion = 0;
  let discovery = 'loading';
  let updatePicker;
  const selection = el('strong', { class: 'chat-model-name' });
  const context = el('small', { class: 'muted' });
  const change = button('Choose model', () => openPicker());
  const settingsButton = button('Settings', () => openSettings());
  const feedback = el('div', { 'aria-live': 'polite' });
  const summary = el('span', { class: 'chat-settings-summary muted' });
  const listing = el('section', { class: 'entity-list chat-card' },
    el('div', { class: 'chat-toolbar' }, el('div', { class: 'chat-selection' }, selection, context),
      change, settingsButton), summary, feedback, session.canvas);
  const workspace = el('div', { class: 'entity-workspace chat-workspace' }, listing);
  const refresh = button('Refresh models', () => loadModels());
  const newChat = button('New chat', () => {
    if (!p.current() || !session.view || (session.view.hasMessages || session.view.hasDraft) && !confirm('Clear this conversation, draft and settings?') || !closePanel()) return;
    session.view.reset(); session.settings = { ...defaultSettings(), node: p.computeNode || '' }; p.selectSection?.(); sync();
  });
  p.root.append(heading('Chat', 'Your models. Your prompts. One place to try them.', refresh, newChat), workspace);
  const modelContext = model => model?.owned_by === 'zzrouter-route' ? 'Route' : [model?.zzrouter?.provider, model?.zzrouter?.node].filter(Boolean).join(' · ');
  const sync = () => {
    const settings = session.settings;
    selection.textContent = settings.model || 'Select a model or route';
    context.textContent = modelContext(models.find(item => item.id === settings.model));
    selection.setAttribute('title', selection.textContent); context.setAttribute('title', context.textContent);
    change.textContent = settings.model ? 'Change' : 'Choose model';
    summary.textContent = [settings.system ? 'Custom system prompt' : '',
      settings.temperature === undefined ? '' : `Temperature ${settings.temperature}`,
      settings.topP === undefined ? '' : `Top-p ${settings.topP}`,
      settings.maxTokens ? `Up to ${settings.maxTokens.toLocaleString()} output tokens` : '',
      settings.temperature === undefined && settings.topP === undefined && settings.maxTokens === undefined ? 'Model defaults' : '',
      settings.node ? `Node: ${settings.node}` : ''].filter(Boolean).join(' · ');
    session.view?.configure(settings);
    p.syncActivity?.();
  };
  function setTarget(model, node) {
    if (session.settings.model && session.settings.model !== model) {
      session.view?.reset();
      session.settings = defaultSettings();
    }
    session.settings = { ...session.settings, model, node };
  }
  function choose(value, node = '') {
    const model = value.trim();
    if (!p.current() || !model || !closePanel()) return;
    setTarget(model, p.computeNode || node);
    p.selectSection?.(model, node || undefined); sync();
  }
  p.handleSelection?.((value, node = '') => { if (value && (value !== session.settings.model || node !== session.settings.node)) choose(value, node); });
  p.scope.signal.addEventListener('abort', () => { session.view?.cancel(); }, { once: true });
  async function loadModels() {
    const version = ++readVersion;
    refresh.disabled = true;
    discovery = 'loading'; feedback.replaceChildren(message('Loading available models…')); updatePicker?.();
    try {
      const result = await p.api.request('/v1/models', { signal: p.scope.signal });
      if (!p.current() || version !== readVersion) return;
      models = chatModels(result).filter(model => !p.computeNode || model.zzrouter?.node === p.computeNode);
      discovery = 'ready';
      feedback.replaceChildren(...(models.length ? [] : [message('No chat models are available for your account.'),
        leaveButton(p.identity.role === 'admin' ? 'Manage providers' : 'Review my access', () => p.go(p.identity.role === 'admin' ? 'providers' : 'connections'), p.api)]));
      sync(); updatePicker?.();
    } catch (error) { if (p.current() && version === readVersion) { discovery = 'failed'; feedback.replaceChildren(problem(error)); updatePicker?.(); } }
    finally { if (p.current() && version === readVersion) refresh.disabled = false; }
  }
  function closePanel() {
    if (drawer && !drawer.panel.confirmLeave()) return false;
    drawer?.dispose(true); drawer = undefined; updatePicker = undefined;
    return true;
  }
  function openPicker() {
    if (!p.current() || !closePanel()) return;
    change.focus({ preventScroll: true });
    drawer = details({ ...p, root: workspace }, 'Choose model or route', closePanel);
    const search = input('model-search', '', { type: 'search', placeholder: 'Search names, providers or nodes', spellcheck: 'false' });
    const results = el('div', { class: 'model-results' });
    const exact = button('', () => choose(search.value), 'secondary');
    updatePicker = () => {
      const query = search.value.trim();
      const matches = models.filter(model => `${model.id} ${modelContext(model)}`.toLowerCase().includes(query.toLowerCase()));
      results.replaceChildren(...matches.map(model => button(el('span', {}, el('strong', {}, model.id), el('small', {}, modelContext(model))), () => choose(model.id), 'model-result')));
      if (!matches.length) results.append(text(discovery === 'loading' ? 'Loading models…' : discovery === 'failed' ? 'Model discovery failed. Refresh models or use an exact ID.' : 'No matching models or routes.', 'muted'));
      exact.hidden = !query || models.some(model => model.id === query);
      exact.textContent = `Use exact ID: ${query}`;
    };
    search.addEventListener('input', updatePicker);
    search.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); results.querySelector?.('button')?.focus(); } });
    drawer.root.append(field('Find a model or route', search), text(session.settings.model ?
      'Choosing a different model or route starts a new chat.' : 'Choose a listed model or enter an exact ID.', 'muted'), results, exact);
    updatePicker(); search.focus({ preventScroll: true });
  }
  function openSettings() {
    if (!p.current() || !closePanel()) return;
    settingsButton.focus({ preventScroll: true });
    drawer = details({ ...p, root: workspace }, 'Chat settings', closePanel);
    const owner = drawer;
    const settings = session.settings;
    const system = el('textarea', { name: 'system', rows: '7', value: settings.system, 'data-sensitive': true, placeholder: 'Give the model instructions for this conversation…' });
    const numbers = [
      ['temperature', 'Temperature', 0, 2, '0.1'],
      ['topP', 'Top-p', 0, 1, '0.05'],
      ['maxTokens', 'Output-token limit', 1, undefined, '1'],
    ].map(([key, label, min, max, step]) => ({ key, control: input(key, settings[key] ?? '', { type: 'number', min, max, step, placeholder: 'Model default' }), label }));
    const nodeNames = [...new Set(models.map(item => item.zzrouter?.node).filter(Boolean))].sort();
    if (settings.node && !nodeNames.includes(settings.node)) nodeNames.push(settings.node);
    const node = select('node', p.computeNode ? [[p.computeNode, p.computeNode]] : [['', 'Automatic routing'], ...nodeNames.map(name => [name, name])], p.computeNode || settings.node);
    node.disabled = Boolean(p.computeNode);
    const reasoning = input('reasoning', '', { type: 'checkbox', checked: settings.showReasoning });
    const stats = input('stats', '', { type: 'checkbox', checked: settings.showStats });
    const apply = el('button', { type: 'submit', class: 'primary' }, 'Apply settings');
    const formStatus = el('div', { 'aria-live': 'polite' });
    const form = el('form', { class: 'entity-editor chat-settings' }, formStatus,
      field('System prompt', system, 'Sent with each message in this conversation.'),
      el('h3', {}, 'Generation'), text('Leave values blank to use the model’s defaults. Changes apply to your next request.', 'muted'),
      el('div', { class: 'form-grid' }, numbers.map(item => field(item.label, item.control))),
      field('Compute node', node, p.computeNode ? 'Testing on this computer. Use Show all computers to leave this setup.' : 'Optional node name. Model IDs may already select a node. Routes remain subject to your team’s access rules.'),
      el('h3', {}, 'Display'), field('Show returned reasoning', reasoning), field('Show response statistics', stats));
    formFeedback(form, formStatus);
    const draft = owner.trackDraft(form);
    form.append(editorActions(apply, closePanel, p.api, draft));
    form.addEventListener('submit', event => {
      event.preventDefault();
      if (!form.reportValidity() || !owner.current()) return;
      session.settings = { ...settings, system: system.value, node: p.computeNode || node.value, showReasoning: reasoning.checked, showStats: stats.checked,
        ...Object.fromEntries(numbers.map(({ key, control }) => [key, control.value === '' ? undefined : Number(control.value)])) };
      draft.clean(); sync(); closePanel();
    });
    owner.root.append(form);
    system.focus({ preventScroll: true });
  }
  if (p.computeNode) {
    setTarget(initialModel || (session.settings.node === p.computeNode ? session.settings.model : ''), p.computeNode);
  } else if (initialModel && (initialModel !== session.settings.model || initialNode !== undefined)) {
    setTarget(initialModel, initialNode || '');
  }
  sync();
  const loading = loadModels();
  await session.ready;
  if (!p.current()) return;
  sync();
  await loading;
}
