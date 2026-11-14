import { el, text, button, heading, field, input, select, message, problem, editorActions } from './dom.mjs';
import { details } from './details.mjs';

export const defaultSettings = () => ({ model: '', system: '', node: '', showReasoning: true, showStats: true });

export function chatModels(result) {
  if (!Array.isArray(result?.data)) throw new Error('The coordinator returned an invalid model list.');
  return result.data.filter(model => typeof model.id === 'string' && model.capabilities?.chat !== false)
    .sort((a, b) => a.id.localeCompare(b.id));
}

export async function chat(p) {
  let settings = defaultSettings();
  let models = [];
  let drawer;
  let view;
  let readVersion = 0;
  const model = input('model', '', { list: 'chat-models', placeholder: 'Search models and routes…', spellcheck: 'false' });
  const choices = el('datalist', { id: 'chat-models' });
  const feedback = el('div', { 'aria-live': 'polite' });
  const summary = el('span', { class: 'chat-settings-summary muted' });
  const canvas = el('div', { class: 'chat-canvas' });
  const listing = el('section', { class: 'entity-list chat-card' },
    el('div', { class: 'chat-toolbar' }, field('Model or route', model), choices,
      button('Settings', () => openSettings())), summary, feedback, canvas);
  const workspace = el('div', { class: 'entity-workspace chat-workspace' }, listing);
  const refresh = button('Refresh models', () => loadModels());
  const newChat = button('New chat', () => {
    if (!p.current() || !view || view.hasMessages && !confirm('Clear this conversation and start a new chat?')) return;
    view.reset();
  });
  p.root.append(heading('Chat', 'Your models. Your prompts. One place to try them.', refresh, newChat), workspace);
  model.addEventListener('change', () => { settings = { ...settings, model: model.value.trim(), node: '' }; sync(); });
  const sync = () => {
    summary.textContent = [settings.system ? 'Custom system prompt' : 'No system prompt',
      settings.temperature === undefined ? '' : `Temperature ${settings.temperature}`,
      settings.topP === undefined ? '' : `Top-p ${settings.topP}`,
      settings.maxTokens ? `Up to ${settings.maxTokens.toLocaleString()} output tokens` : '',
      settings.temperature === undefined && settings.topP === undefined && settings.maxTokens === undefined ? 'Model defaults' : '',
      settings.node ? `Node: ${settings.node}` : ''].filter(Boolean).join(' · ');
    view?.configure(settings);
  };
  async function loadModels() {
    const version = ++readVersion;
    refresh.disabled = true;
    feedback.replaceChildren(message('Loading available models…'));
    try {
      const result = await p.api.request('/v1/models', { signal: p.scope.signal });
      if (!p.current() || version !== readVersion) return;
      models = chatModels(result);
      choices.replaceChildren(...models.map(item => el('option', { value: item.id }, item.zzrouter?.provider || 'Route')));
      feedback.replaceChildren(models.length ? text(`${models.length} available. Type to search or enter an exact model name.`, 'chat-model-help muted') : message('No chat models are listed for your key. Refresh after a model or route is made available.'));
    } catch (error) { if (p.current() && version === readVersion) feedback.replaceChildren(problem(error)); }
    finally { if (p.current() && version === readVersion) refresh.disabled = false; }
  }
  function closeSettings() {
    if (!drawer?.panel.confirmLeave()) return;
    drawer.dispose(true); drawer = undefined;
  }
  function openSettings() {
    if (!p.current() || drawer?.current()) return;
    drawer = details({ ...p, root: workspace }, 'Chat settings', closeSettings);
    const owner = drawer;
    const system = el('textarea', { name: 'system', rows: '7', value: settings.system, 'data-sensitive': true, placeholder: 'Give the model instructions for this conversation…' });
    const numbers = [
      ['temperature', 'Temperature', 0, 2, '0.1'],
      ['topP', 'Top-p', 0, 1, '0.05'],
      ['maxTokens', 'Output-token limit', 1, undefined, '1'],
    ].map(([key, label, min, max, step]) => ({ key, control: input(key, settings[key] ?? '', { type: 'number', min, max, step, placeholder: 'Model default' }), label }));
    const nodeNames = [...new Set(models.map(item => item.zzrouter?.node).filter(Boolean))].sort();
    if (settings.node && !nodeNames.includes(settings.node)) nodeNames.push(settings.node);
    const node = select('node', [['', 'Automatic routing'], ...nodeNames.map(name => [name, name])], settings.node);
    const reasoning = input('reasoning', '', { type: 'checkbox', checked: settings.showReasoning });
    const stats = input('stats', '', { type: 'checkbox', checked: settings.showStats });
    const apply = el('button', { type: 'submit', class: 'primary' }, 'Apply settings');
    const form = el('form', { class: 'entity-editor chat-settings' },
      field('System prompt', system, 'Sent with each message in this conversation.'),
      el('h3', {}, 'Generation'), text('Leave values blank to use the model’s defaults. Changes apply to your next request.', 'muted'),
      el('div', { class: 'form-grid' }, numbers.map(item => field(item.label, item.control))),
      field('Compute node', node, 'Optional node name. Model IDs may already select a node. Routes remain subject to your team’s access rules.'),
      el('h3', {}, 'Display'), field('Show returned reasoning', reasoning), field('Show response statistics', stats),
      editorActions(apply, closeSettings, p.api));
    const draft = owner.trackDraft(form);
    form.addEventListener('submit', event => {
      event.preventDefault();
      if (!form.reportValidity() || !owner.current()) return;
      settings = { ...settings, system: system.value, node: node.value, showReasoning: reasoning.checked, showStats: stats.checked,
        ...Object.fromEntries(numbers.map(({ key, control }) => [key, control.value === '' ? undefined : Number(control.value)])) };
      draft.clean(); sync(); closeSettings();
    });
    owner.root.append(form);
    system.focus({ preventScroll: true });
  }
  sync();
  const loading = loadModels();
  const { mountChat } = await import('./vendor/chat.mjs');
  if (!p.current()) return;
  view = mountChat(canvas, { baseURL: new URL('/v1', p.api.origin).href, fetcher: p.api.inferenceFetch.bind(p.api), settings, signal: p.scope.signal });
  await loading;
}
