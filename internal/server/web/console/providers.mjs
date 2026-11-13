import { details, readAfterWrite } from './details.mjs';
import { applySummary } from './api.mjs';
import { el, text, button, leaveButton, badge, actions, message, card, heading, field, input, facts, clearSecrets, problem, markSelection, editorActions } from './dom.mjs';
import { providerCategory, readInventory, nodeInventory } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';

export async function providers(p, category = 'local', selectedKey) {
  const categories = [['local', 'Local providers'], ['cloud', 'Cloud providers'], ['registries', 'Model registries']];
  if (!categories.some(([id]) => id === category)) category = 'local';
  let catalog, inventory, drawer;
  const list = el('div', { class: 'node-grid provider-grid', 'aria-label': 'Providers' });
  const listing = el('div', { class: 'entity-list' }, list);
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  const choose = async section => {
    const [, ...key] = (section || category).split('/');
    selectedKey = key.join('/');
    drawer?.dispose(!selectedKey); drawer = undefined;
    markSelection(list, selectedKey);
    if (!selectedKey) return;
    const entry = catalog.find(item => item.key === selectedKey && providerCategory(item) === category);
    drawer = details({ ...p, root: workspace }, 'Provider details', () => p.go('providers', category));
    if (!entry) { drawer.root.append(message('This provider is not defined on the coordinator. Providers reported only by a worker are configured on that node.', 'warning')); return; }
    await showProvider(drawer, entry, inventory, async status => {
      await read();
      if (!p.current()) return;
      const item = [...list.children].find(item => item.dataset.provider === entry.key);
      if (category === 'cloud' && item) item.children[0].children[1].textContent = status.configured ? 'Configured' : 'Not configured';
    });
  };
  const read = async () => {
    const fresh = await Promise.all([p.api.list('/providers/catalog?kind=all', { signal: p.scope.signal }), category !== 'registries' ? readInventory(p) : null]);
    if (!p.current()) return;
    [catalog, inventory] = fresh;
    const entries = catalog.filter(entry => providerCategory(entry) === category);
    list.replaceChildren(...entries.map(entry => {
      const item = leaveButton('', () => p.go('providers', `${category}/${entry.key}`), p.api, 'network-item');
      item.setAttribute('data-selection', entry.key);
      item.dataset.provider = entry.key;
      item.append(el('span', { class: 'network-item-title' }, providerName(entry.key, entry.name), badge(category === 'cloud' ? entry.available ? 'Configured' : 'Not configured' : category === 'registries' ? 'Registry' : 'Local')),
        text(entry.description || (category === 'local' ? 'Inspect provider instances on your nodes.' : 'Inspect connection settings.'), 'muted'));
      return item;
    }));
    if (!entries.length) list.append(message('No provider definitions in this category. Add provider configuration on the coordinator, then refresh.'));
    markSelection(list, selectedKey);
  };
  const refresh = async () => {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    const panel = drawer; const revision = panel?.panel.revision;
    const unchanged = () => p.current() && p.api.canLeave() && drawer === panel && drawer?.panel.revision === revision;
    try { await read(); if (unchanged()) await choose(selectedKey ? `${category}/${selectedKey}` : category); }
    catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  const tabs = el('nav', { class: 'actions tabs', 'aria-label': 'Provider categories' }, categories.map(([id, label]) => {
    const item = leaveButton(label, () => p.go('providers', id), p.api);
    item.setAttribute('aria-current', String(id === category)); return item;
  }));
  p.root.append(heading('Providers', 'Local providers on your nodes, cloud providers and model registries.', leaveButton('Refresh', refresh, p.api)), tabs, workspace);
  list.append(message('Loading provider catalog…'));
  await read();
  if (!p.current()) return;
  p.handleSelection?.(choose, section => (section || 'local').split('/')[0] === category);
  if (selectedKey) await choose(`${category}/${selectedKey}`);
}

// Used by both the map and provider list; credentials remain coordinator-scoped.
export async function showProvider(p, entry, inventory, onSaved) {
  const load = async (notice, tone) => {
    const revision = p.panel.revision;
    const current = () => p.panel.current() && p.panel.revision === revision && p.api.canLeave();
    const status = entry.connection_supported ? await p.api.data(`/providers/${encodeURIComponent(entry.key)}/connection`, { signal: p.panel.signal }) : null;
    if (!current()) return;
    if (notice && onSaved) await onSaved(status);
    if (!current()) return;
    const show = () => {
      const reader = p.panel.view('Provider details');
      reader.root.append(el('div', { class: 'detail-heading' }, providerName(entry.key, entry.name, 'h2'), status ? leaveButton(status.configured ? 'Edit' : 'Connect', edit, p.api, 'primary') : null),
        notice ? message(notice, tone) : '', text(entry.description || (providerCategory(entry) === 'cloud' ? 'Cloud provider connection' : 'Local provider'), 'muted'));
      if (status) reader.root.append(actions(badge(status.configured ? 'Configured' : 'Not configured', status.configured ? 'good' : 'warn'), badge(status.enabled ? 'Enabled' : 'Disabled')),
        facts([['Configured on', p.state.node_name], ['Credential source', status.mode === 'managed' ? 'Protected store' : status.mode === 'external' ? 'Installation configuration' : status.mode],
          ...status.fields.map(spec => [spec.label, spec.secret ? status.present?.includes(spec.id) ? 'Present' : 'Not set' : status.values?.[spec.id] || spec.default || 'Not set'])]),
        text('Configuration is not a live provider health check. These connection settings apply to the coordinator only.', 'muted'));
      if (inventory && providerCategory(entry) === 'cloud') {
        const locations = new Set([...(inventory[1].value || []).filter(row => row.name === entry.key && row.enabled).map(row => row.node), ...(inventory[0].value || []).filter(node => node.providers?.some(provider => provider.key === entry.key)).map(node => node.name)]);
        reader.root.append(facts([['Configuration', status ? status.configured ? 'Configured' : 'Not configured' : entry.available ? 'Configured' : 'Not configured'], ['Reported on', [...locations].join(', ') || (inventory.some(result => result.status === 'rejected') ? 'Node enablement unavailable' : 'No enabled node reported')]]));
      }
      if (inventory && providerCategory(entry) === 'local') reader.root.append(el('section', { class: 'access-section' }, el('h3', {}, 'Compute nodes'), nodeInventory(reader, inventory, entry)));
      if (!status) reader.root.append(message(providerCategory(entry) === 'local' ? 'Use the terminal client for installation and runtime setup. Select a node to manage its enabled local providers.' : 'No editable connection fields are declared. Configure this connection on the coordinator.'));
    };
    const edit = () => {
      const editor = p.panel.view(status.configured ? 'Edit provider' : 'Connect provider');
      editor.root.append(connectionForm(editor, entry, status, async (notice, tone) => {
        // Read both summaries before replacing the editor. No write is retried.
        if (inventory) inventory = await readInventory(editor);
        await load(notice, tone);
      }, show));
      editor.root.querySelector?.('input')?.focus({ preventScroll: true });
      p.syncActivity();
    };
    show();
  };
  p.root.append(message('Loading provider details…'));
  try { await load(); }
  catch (error) { if (p.current()) p.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), problem(error), leaveButton('Reload details', () => showProvider(p, entry, inventory, onSaved), p.api), inventory ? nodeInventory(p, inventory, entry) : ''); }
}

function connectionForm(p, entry, status, reload, cancel) {
  const form = el('form', { autocomplete: 'off', class: 'entity-editor' });
  const draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const feedback = el('div', { 'aria-live': 'polite' });
  const diagnostic = el('div', { 'aria-live': 'polite' });
  const values = new Map();
  let blocked = false;
  let tested = false;
  const writeButtons = [];
  for (const spec of status.fields) {
    const value = spec.secret ? '' : (status.values?.[spec.id] ?? spec.default ?? '');
    const element = input(spec.id, value, { type: spec.secret ? 'password' : spec.type === 'endpoint' ? 'url' : 'text', required: spec.required && !spec.default, maxlength: '16384', spellcheck: 'false' });
    values.set(spec.id, element);
    element.addEventListener('input', () => {
      if (tested) diagnostic.replaceChildren(message('The candidate changed. Test again to verify these exact values.'));
      tested = false;
    });
    const hint = spec.secret ? (status.present?.includes(spec.id) ? 'A stored or external value is present. Enter a new value to test or replace it.' : spec.required ? 'Required. The saved value cannot be read back.' : 'Optional. Leave blank for public, anonymous access.') : spec.type === 'endpoint' ? 'HTTPS required; HTTP is permitted for loopback services.' : '';
    controls.append(field(spec.label + (spec.required ? ' *' : ''), element, hint));
  }
  const candidate = () => Object.fromEntries([...values].map(([key, element]) => [key, element.value]));
  const resetBusy = () => {
    controls.disabled = false;
    for (const item of writeButtons) item.disabled = blocked || !status.managed_available;
  };
  const test = async () => {
    if (!form.reportValidity() || controls.disabled || !p.current() || !p.api.canChange()) return;
    controls.disabled = true;
    diagnostic.replaceChildren(message('Testing this candidate. Nothing is being saved.'));
    try {
      const result = await p.api.data(`/providers/${encodeURIComponent(entry.key)}/connection/test`, { method: 'POST', body: { values: candidate() }, signal: p.scope.signal, effectful: false });
      if (!p.current()) return;
      tested = true;
      const success = result.status === 'ok';
      const details = success ? 'The upstream accepted this exact candidate. It has not been saved.' : `Test failed: ${String(result.code).replaceAll('_', ' ')}${result.http_status ? ` (HTTP ${result.http_status})` : ''}. Nothing was saved.`;
      diagnostic.replaceChildren(message(details, success ? '' : 'warning'));
      if (entry.key === 'huggingface') diagnostic.append(text('This token check does not prove access to every gated repository. Repository approval may also be required; public browsing can work without a token.', 'muted'));
    } catch (error) {
      if (p.current()) diagnostic.replaceChildren(message(`The diagnostic did not complete. No connection was saved. ${error.status ? `HTTP ${error.status}.` : 'Check connectivity and test again.'}`, 'error'));
    } finally { if (p.current()) resetBusy(); }
  };
  const change = async (operation, activate = false) => {
    if (blocked || controls.disabled || !p.current() || !p.api.canChange()) return;
    if (operation === 'save' && !form.reportValidity()) return;
    if (operation === 'remove' && !confirm('Remove managed credentials and disable external credential fallback for this connection?')) return;
    if (operation === 'external' && !confirm('Discard managed credentials and resume this coordinator’s declared environment or credential-cache values?')) return;
    if (!p.current()) return;
    controls.disabled = true;
    feedback.replaceChildren(message('Applying connection change…'));
    try {
      const path = `/providers/${encodeURIComponent(entry.key)}/connection` + (operation === 'external' ? '/use-external' : '');
      const result = await p.api.data(path, {
        method: operation === 'save' ? 'PUT' : operation === 'remove' ? 'DELETE' : 'POST', signal: p.scope.signal,
        body: { expected_revision: status.revision, ...(operation === 'save' ? { values: candidate(), activate } : {}) },
      });
      if (!p.current()) return;
      clearSecrets(form);
      blocked = true;
      draft?.clean();
      await readAfterWrite(p, reload, applySummary(result), result.outcome === 'saved' && result.durable && result.published ? '' : 'warning');
    } catch (error) {
      if (!p.current()) return;
      blocked = error.uncertain || error.status === 409;
      if (error.uncertain) clearSecrets(form);
      feedback.replaceChildren(problem(error));
      if (blocked) feedback.append(message('Reload this connection before another change. Your previous revision will not be retried.', 'warning'), leaveButton('Reload details', () => { draft?.clean(); void readAfterWrite(p, reload, 'Reloading connection state.'); }, p.api));
    } finally { if (p.current()) resetBusy(); }
  };
  const activate = input('activate', '', { type: 'checkbox' });
  const save = el('button', { type: 'submit', class: 'primary' }, 'Save changes');
  const remove = button('Remove connection', () => change('remove'), 'danger');
  const external = button('Use installation values', () => change('external'));
  writeButtons.push(save, remove, external);
  controls.prepend(providerName(entry.key, entry.name, 'h2'),
    !status.managed_available ? message('Saving is unavailable. Configure security.credentials.key_file on the coordinator and restart it. Candidate tests remain available.', 'warning') : text('Enter a complete replacement, including each required secret. Saved secrets are never filled into this form.', 'muted'));
  if (entry.is_cloud) controls.append(el('label', { class: 'model-choice' }, activate, 'Connect after saving'));
  controls.append(actions(button('Test candidate', test)), diagnostic,
    el('details', { class: 'access-section danger-zone' }, el('summary', {}, 'Connection management'), actions(remove, external)));
  form.append(controls, feedback, editorActions(save, () => { if (!draft || draft.discard()) { draft?.clean(); cancel(); } }, p.api));
  form.addEventListener('submit', event => { event.preventDefault(); void change('save', entry.is_cloud && activate.checked); });
  resetBusy();
  return form;
}
