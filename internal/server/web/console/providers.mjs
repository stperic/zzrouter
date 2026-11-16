import { details, formSubmission, readAfterWrite } from './details.mjs';
import { applySummary } from './api.mjs';
import { el, text, button, leaveButton, badge, actions, message, heading, field, input, facts, clearSecrets, problem, markSelection, editorActions, formFeedback } from './dom.mjs';
import { providerCategory, readInventory, nodeInventory, providerObservationSummary } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';
import { providerControls, remoteOllama } from './provider-lifecycle.mjs';
import { parameterSettings } from './parameters.mjs';

export async function providers(p, category = 'local', selectedKey, mode) {
  const categories = [['local', 'Local providers'], ['cloud', 'Cloud providers'], ['registries', 'Model registries']];
  if (!categories.some(([id]) => id === category)) category = 'local';
  let catalog, inventory, drawer;
  const list = el('div', { class: 'node-grid provider-grid', 'aria-label': 'Providers' });
  const listing = el('div', { class: 'entity-list' }, list);
  const workspace = el('div', { class: 'entity-workspace provider-workspace' }, listing);
  const choose = async (section, nextMode) => {
    const [, ...key] = (section || category).split('/');
    selectedKey = key.join('/');
    drawer?.dispose(!selectedKey); drawer = undefined;
    markSelection(list, selectedKey);
    if (category === 'local' && nextMode === 'create') {
      drawer = details({ ...p, root: workspace }, 'Connect remote Ollama', () => p.go('providers', category));
      remoteOllama(drawer, async key => { await read(); if (p.current()) await p.go('providers', key ? `${category}/${key}` : category); });
      return;
    }
    if (!selectedKey) return;
    const entry = catalog.find(item => item.key === selectedKey && providerCategory(item) === category);
    drawer = details({ ...p, root: workspace }, 'Provider details', () => p.go('providers', category));
    if (!entry) { drawer.root.append(message('This provider is not defined on the coordinator. Providers reported only by a worker are configured on that node.', 'warning')); return; }
    await showProvider(drawer, entry, inventory, read);
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
      item.append(el('span', { class: 'network-item-title' }, providerName(entry.key, entry.name), badge(category === 'cloud' ? entry.available ? 'Configured' : 'Set up' : category === 'registries' ? 'Registry' : 'Local', category === 'cloud' && entry.available ? 'good' : '')),
        text(providerObservationSummary(entry, inventory), 'muted'));
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
  const description = { local: 'Manage the providers installed on your compute nodes.', cloud: 'Choose a provider to add an API key or update its connection.', registries: 'Add credentials for private or gated models. Public models can be browsed without a token.' };
  p.root.append(heading('Providers', description[category], leaveButton('Refresh', refresh, p.api), category === 'local' ? leaveButton('+ Connect remote Ollama', () => p.go('providers', category, 'create'), p.api, 'primary') : null), tabs, workspace);
  list.append(message('Loading provider catalog…'));
  await read();
  if (!p.current()) return;
  p.handleSelection?.(choose, section => (section || 'local').split('/')[0] === category);
  if (selectedKey || mode === 'create') await choose(selectedKey ? `${category}/${selectedKey}` : category, mode);
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
    const reload = owner => async (notice, tone) => {
      if (inventory) inventory = await readInventory(owner);
      await load(notice, tone);
    };
    const show = () => {
      const reader = p.panel.view('Provider details');
      reader.root.append(el('div', { class: 'detail-heading' }, providerName(entry.key, entry.name, 'h2'), status ? leaveButton(status.configured ? 'Edit' : 'Connect', edit, p.api, entry.is_cloud && status.configured && !status.enabled ? 'secondary' : 'primary') : null),
        notice ? message(notice, tone) : '', text(entry.description || (providerCategory(entry) === 'cloud' ? 'Cloud provider connection' : 'Local provider'), 'muted'));
      const connectionDetails = status ? el('details', { class: 'provider-advanced' }, el('summary', {}, 'Connection details'),
        facts([['Coordinator', p.state.node_name], ['Credential source', status.mode === 'managed' ? 'Encrypted storage' : status.mode === 'external' ? 'Installation configuration' : 'External credentials blocked']]),
        text('Configuration status does not verify access to the provider.', 'muted')) : null;
      if (status) reader.root.append(actions(badge(status.configured ? 'Configured' : 'Not configured', status.configured ? 'good' : ''), entry.is_cloud && status.configured ? badge(status.enabled ? 'Enabled' : 'Disabled') : null),
        facts(status.fields.map(spec => [spec.label, spec.secret ? status.present?.includes(spec.id) ? status.mode === 'managed' ? 'Saved' : 'Provided by installation' : 'Not set' : status.values?.[spec.id] || spec.default || 'Not set'])));
      if (entry.is_cloud && status?.configured) {
        const enabling = !status.enabled;
        const enable = button(enabling ? 'Enable provider' : 'Disable provider', async () => {
          if (enable.disabled || !reader.current() || !p.api.canChange()) return;
          enable.disabled = true;
          try {
            await p.api.data(`/providers/${encodeURIComponent(entry.key)}`, { method: 'PATCH', body: { enabled: enabling }, signal: reader.scope.signal });
            if (reader.current()) await readAfterWrite(reader, reload(reader), enabling ? 'Provider enabled.' : 'Provider disabled.');
          } catch (error) {
            if (reader.current()) await readAfterWrite(reader, reload(reader), problem(error), 'warning');
          }
        }, 'primary');
        enable.setAttribute('data-protected-actions', '');
        reader.root.append(actions(enable));
      }
      if (!status && entry.is_cloud) reader.root.append(actions(badge(entry.available ? 'Configured' : 'Not configured')));
      if (inventory && providerCategory(entry) === 'cloud') {
        const locations = new Set([...(inventory[1].value || []).filter(row => row.name === entry.key && row.enabled).map(row => row.node), ...(inventory[0].value || []).filter(node => node.providers?.some(provider => provider.key === entry.key)).map(node => node.name)]);
        (connectionDetails || reader.root).append(facts([['Enabled on', [...locations].join(', ') || (inventory.some(result => result.status === 'rejected') ? 'Node enablement unavailable' : 'No enabled nodes')]]));
      }
      if (connectionDetails) reader.root.append(connectionDetails);
      if (inventory && providerCategory(entry) === 'local') reader.root.append(providerControls(reader, entry, inventory, show), el('section', { class: 'access-section' }, el('h3', {}, 'Compute nodes'), nodeInventory(reader, inventory, entry)));
      if (providerCategory(entry) === 'cloud') reader.root.append(leaveButton('Provider defaults', () => parameterSettings(reader.panel.view('Provider parameters'), { provider: entry.key, back: show }), p.api));
      if (!status && providerCategory(entry) !== 'local') reader.root.append(message('No editable connection fields are declared. Configure this connection on the coordinator.'));
    };
    const edit = () => {
      const editor = p.panel.view(status.configured ? 'Edit provider' : 'Connect provider');
      editor.root.append(connectionForm(editor, entry, status, reload(editor), show));
      editor.root.querySelector?.('input')?.focus({ preventScroll: true });
      p.syncActivity();
    };
    if (status && !status.configured && providerCategory(entry) !== 'local' && !notice) edit();
    else show();
  };
  p.root.append(message('Loading provider details…'));
  try { await load(); }
  catch (error) { if (p.current()) p.root.replaceChildren(providerName(entry.key, entry.name, 'h2'), problem(error), leaveButton('Reload details', () => showProvider(p, entry, inventory, onSaved), p.api), ...(inventory && providerCategory(entry) === 'local' ? [providerControls(p, entry, inventory, () => showProvider(p, entry, inventory, onSaved)), nodeInventory(p, inventory, entry)] : [])); }
}

function connectionForm(p, entry, status, reload, cancel) {
  const form = el('form', { autocomplete: 'off', class: 'entity-editor provider-editor' });
  const draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const feedback = el('div', { 'aria-live': 'polite' });
  const diagnostic = el('div', { 'aria-live': 'polite' });
  const values = new Map();
  const validation = formFeedback(form, feedback, key => values.get(key) || values.get(String(key).replace(/^values\./, '')));
  let tested = false;
  const writeButtons = [];
  const enable = entry.is_cloud && !status.enabled;
  const saveLabel = enable ? 'Save and enable' : 'Save changes';
  controls.append(providerName(entry.key, entry.name, 'h2'),
    text(status.configured ? 'Enter replacement credentials. Existing keys stay hidden.' : 'Enter your provider credentials. You can test them before saving.', 'muted'));
  for (const spec of status.fields) {
    const value = spec.secret ? '' : (status.values?.[spec.id] ?? spec.default ?? '');
    const element = input(spec.id, value, { type: spec.secret ? 'password' : spec.type === 'endpoint' ? 'url' : 'text', required: spec.required && !spec.default, maxlength: '16384', spellcheck: 'false', 'data-sensitive': spec.secret, autocapitalize: 'none' });
    values.set(spec.id, element);
    element.addEventListener('input', () => {
      if (tested) diagnostic.replaceChildren(message('Credentials changed. Test again to check the new values.'));
      tested = false;
    });
    const hint = spec.secret ? (spec.required ? 'Stored encrypted on this coordinator after saving.' : 'Optional. Leave blank to use public models without a token.') : spec.type === 'endpoint' ? 'Use HTTPS, or HTTP for a service on this computer.' : '';
    const label = spec.label + (spec.required ? ' *' : ' (optional)');
    if (spec.secret) {
      const id = `provider-${entry.key}-${spec.id}`;
      element.setAttribute('id', id);
      element.setAttribute('aria-label', label);
      element.setAttribute('aria-describedby', `${id}-hint`);
      const reveal = button('Show', () => {
        const showing = element.type === 'password';
        element.type = showing ? 'text' : 'password';
        reveal.textContent = showing ? 'Hide' : 'Show';
        reveal.setAttribute('aria-label', `${showing ? 'Hide' : 'Show'} ${spec.label.toLowerCase()}`);
        reveal.setAttribute('aria-pressed', String(showing));
      });
      reveal.setAttribute('aria-label', `Show ${spec.label.toLowerCase()}`);
      reveal.setAttribute('aria-pressed', 'false');
      controls.append(el('div', { class: 'field provider-secret-field' }, el('label', { for: id }, label),
        el('div', { class: 'provider-secret-control' }, element, reveal), el('small', { id: `${id}-hint` }, hint)));
    } else controls.append(field(label, element, hint));
  }
  const candidate = () => Object.fromEntries([...values].map(([key, element]) => [key, element.value]));
  const resetBusy = () => {
    controls.disabled = submission.blocked;
    form.setAttribute('aria-busy', 'false');
    save.textContent = saveLabel;
    for (const item of writeButtons) item.disabled = submission.blocked || !status.managed_available;
  };
  const test = async () => {
    if (!form.reportValidity() || controls.disabled || !p.current() || !p.api.canChange()) return;
    controls.disabled = true;
    save.disabled = true;
    form.setAttribute('aria-busy', 'true');
    diagnostic.replaceChildren(message('Testing connection…'));
    try {
      const result = await p.api.data(`/providers/${encodeURIComponent(entry.key)}/connection/test`, { method: 'POST', body: { values: candidate() }, signal: p.scope.signal, effectful: false });
      if (!p.current()) return;
      tested = true;
      const success = result.status === 'ok';
      const failures = {
        authentication_failed: 'The provider rejected these credentials. Check that the key is complete, active, and belongs to this provider account.',
        unreachable: 'Could not reach the provider. Check the address and the coordinator’s network connection.',
        invalid_endpoint: 'Check the provider address and try again.',
        rate_limited: 'The provider is limiting requests. Wait a moment and test again.',
        redirect_refused: 'The provider redirected the request. Check its API address; credentials were not sent to the new address.',
      };
      const failure = result.http_status === 403 ? 'The provider denied access. Check the key’s permissions and your account’s access to this service.' : failures[result.code] || 'The provider did not accept the test. Check its settings and try again.';
      diagnostic.replaceChildren(message(success ? 'Connection verified. Save to apply these credentials.' : `${failure}${result.http_status ? ` (HTTP ${result.http_status})` : ''} Nothing was saved.`, success ? '' : 'warning'));
      if (entry.key === 'huggingface') diagnostic.append(text('A valid token may still need approval for a gated model. Public models do not require a token.', 'muted'));
    } catch (error) {
      if (p.current()) diagnostic.replaceChildren(message(`Could not complete the test. Nothing was saved. ${error.status ? `HTTP ${error.status}.` : 'Check connectivity and try again.'}`, 'error'));
    } finally { if (p.current()) resetBusy(); }
  };
  const change = async (operation, activate = false) => {
    if (submission.blocked || !status.managed_available || controls.disabled || !p.current() || !p.api.canChange()) return;
    if (operation === 'save' && !form.reportValidity()) return;
    if (operation === 'remove' && !confirm('Remove these credentials? Credentials from the coordinator installation will also be blocked until you choose Use installation credentials.')) return;
    if (operation === 'external' && !confirm('Replace saved credentials with credentials from the coordinator installation?')) return;
    if (!p.current()) return;
    save.textContent = 'Saving…';
    form.setAttribute('aria-busy', 'true');
    feedback.replaceChildren(message(operation === 'save' ? 'Saving credentials…' : 'Updating connection…'));
    await submission.run(() => {
      const path = `/providers/${encodeURIComponent(entry.key)}/connection` + (operation === 'external' ? '/use-external' : '');
      return p.api.data(path, {
        method: operation === 'save' ? 'PUT' : operation === 'remove' ? 'DELETE' : 'POST', signal: p.scope.signal,
        body: { expected_revision: status.revision, ...(operation === 'save' ? { values: candidate(), activate } : {}) },
      });
    }, async result => {
      clearSecrets(form);
      const complete = result.outcome === 'saved' && result.durable && result.published && (!activate || result.active);
      const summary = operation === 'remove' ? 'Credentials removed.' : operation === 'external' ? 'Using credentials from the coordinator installation.' : activate && result.active ? 'Credentials saved. Provider enabled.' : 'Credentials saved.';
      const receipt = el('div', {}, text(complete ? summary : result.saved ? 'The change was saved, but needs attention. Review the save details before continuing.' : 'The change was not saved. Review the save details before continuing.'),
        el('details', { class: 'provider-advanced' }, el('summary', {}, 'Save details'), text(applySummary(result))));
      await readAfterWrite(p, reload, receipt, complete ? '' : 'warning');
    }, (error, blocked) => {
      if (error.status === 409) submission.block();
      if (error.uncertain || error.mutation_applied) clearSecrets(form);
      validation.error(error);
      if (blocked || submission.blocked) feedback.append(message('Reload this connection before another change. Your previous revision will not be retried.', 'warning'), leaveButton('Reload details', () => { draft?.clean(); void readAfterWrite(p, reload, 'Reloading connection state.'); }, p.api));
    });
    if (p.current()) resetBusy();
  };
  const save = el('button', { type: 'submit', class: 'primary' }, saveLabel);
  const submission = formSubmission(p, controls, save, draft);
  const remove = button('Remove credentials', () => change('remove'), 'danger');
  const external = button('Use installation credentials', () => change('external'));
  writeButtons.push(save, remove, external);
  const advanced = el('details', { class: 'provider-advanced' }, el('summary', {}, 'Advanced settings'));
  if (!status.managed_available) {
    controls.append(message('Saving is unavailable until secure credential storage is set up on the coordinator. You can still test a connection.', 'warning'));
    advanced.append(text('Set security.credentials.key_file on the coordinator to a private encryption-key file, then restart it.', 'muted'));
  }
  if (enable) {
    const saveOnly = button('Save without enabling', () => change('save'));
    writeButtons.push(saveOnly);
    advanced.append(text('Keep this provider disabled while saving its credentials.', 'muted'), actions(saveOnly));
  }
  if (status.configured || status.mode !== 'external') advanced.append(text('Remove credentials and block fallback to installation credentials.', 'muted'), actions(remove));
  if (status.mode !== 'external') advanced.append(text('Discard saved credentials and use this coordinator’s environment or credential cache.', 'muted'), actions(external));
  controls.append(actions(button('Test connection', test)), diagnostic,
    text(enable ? 'Testing does not save anything. Save and enable stores these credentials and enables the provider.' : 'Testing does not save anything. Saving applies these credentials without running a test.', 'muted'));
  if (advanced.children.length > 1) controls.append(advanced);
  form.append(feedback, controls, editorActions(save, cancel, p.api, draft));
  form.addEventListener('submit', event => { event.preventDefault(); void change('save', enable); });
  resetBusy();
  return form;
}
