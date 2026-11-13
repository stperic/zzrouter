import { Lifetime, applySummary } from './api.mjs';
import { el, text, button, leaveButton, badge, actions, message, card, heading, field, input, facts, clearSecrets, problem } from './dom.mjs';

export async function providers(p) {
  p.root.append(heading('Providers', 'Connect cloud services and model registries. Credentials stay on this coordinator.'), message('Loading provider catalog…'));
  const catalog = await p.api.list('/providers/catalog?kind=all', { signal: p.scope.signal });
  if (!p.current()) return;
  const list = el('div', { class: 'item-list', 'aria-label': 'Providers' });
  const detail = el('div');
  const lifetime = new Lifetime();
  p.scope.signal.addEventListener('abort', () => { lifetime.end(); clearSecrets(detail); }, { once: true });
  const choose = async entry => {
    if (!p.current() || !p.api.canLeave()) return;
    clearSecrets(detail);
    const scope = lifetime.next();
    const current = () => p.current() && scope.current();
    const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    for (const item of list.children) item.setAttribute('aria-current', String(item.dataset.provider === entry.key));
    detail.replaceChildren(card(entry.name, text(entry.description || entry.key, 'provider-description')));
    if (!entry.connection_supported) {
      detail.firstChild.append(message('This provider has no editable connection fields. Use Providers in the terminal client for installation and local runtime setup.'));
      return;
    }
    detail.append(message('Reading connection status…'));
    try {
      const status = await p.api.data(`/providers/${encodeURIComponent(entry.key)}/connection`, { signal });
      if (!current()) return;
      detail.replaceChildren(connectionForm({ ...p, current, scope: { signal } }, entry, status, () => choose(entry)));
    } catch (error) {
      if (current()) detail.replaceChildren(card(entry.name, problem(error), leaveButton('Reload connection', () => choose(entry), p.api)));
    } finally { p.syncActivity(); }
  };
  for (const entry of catalog) {
    const item = leaveButton('', () => choose(entry), p.api);
    item.dataset.provider = entry.key;
    item.append(el('span', {}, entry.name), badge(entry.is_cloud ? 'Cloud' : entry.kind === 'registry' ? 'Registry' : 'Local'));
    list.append(item);
  }
  p.root.replaceChildren(heading('Providers', 'Connect cloud services and model registries. Credentials stay on this coordinator.'), el('div', { class: 'split' }, list, detail));
  if (!catalog.length) p.root.append(message('No provider definitions are installed. Add provider configuration on the coordinator, then reload.'));
  else await choose(catalog.find(entry => entry.connection_supported) || catalog[0]);
}

function connectionForm(p, entry, status, reload) {
  const form = el('form', { autocomplete: 'off' });
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
      feedback.replaceChildren(message(applySummary(result), result.outcome === 'saved' && result.durable && result.published ? '' : 'warning'), text('Saving does not perform a network test. Reload the connection to see the current ownership and revision.', 'muted'));
      diagnostic.replaceChildren();
      tested = false;
    } catch (error) {
      if (!p.current()) return;
      blocked = error.uncertain || error.status === 409;
      if (error.uncertain) clearSecrets(form);
      feedback.replaceChildren(problem(error));
      if (blocked) feedback.append(message('Reload this connection before another change. Your previous revision will not be retried.', 'warning'));
    } finally { if (p.current()) resetBusy(); }
  };
  const save = button('Save credentials', () => change('save'), 'primary');
  const connect = entry.is_cloud ? button('Save and connect', () => change('save', true), 'primary') : null;
  const remove = button('Remove', () => change('remove'), 'danger');
  const external = button('Use external values', () => change('external'));
  writeButtons.push(save, remove, external, ...(connect ? [connect] : []));
  controls.append(actions(button('Test candidate', test), save, connect), el('hr'), actions(remove, external));
  form.append(controls);
  form.addEventListener('submit', event => event.preventDefault());
  resetBusy();
  return card(entry.name,
    text(entry.description || entry.key, 'provider-description'),
    actions(badge(status.configured ? 'Configured' : 'Not configured', status.configured ? 'good' : 'warn'), badge(status.enabled ? 'Enabled' : 'Disabled'), badge(status.mode)),
    facts([['Scope', status.scope], ['Revision', status.revision]]),
    !status.managed_available ? message('Saving is unavailable. Configure security.credentials.key_file on the coordinator and restart it. Candidate tests remain available.', 'warning') : null,
    entry.key === 'huggingface' ? text('A token is optional for public models. Testing an empty token can be rejected even when anonymous browsing works. Gated repositories may also require access approval.', 'muted') : null,
    text('Enter a complete replacement, including each required secret. Saved secret values are never filled into this form.', 'muted'),
    form, diagnostic, feedback, actions(leaveButton('Reload connection', reload, p.api)),
  );
}
