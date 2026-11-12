import { changedFields, Lifetime, Obsolete } from './api.mjs';
import { el, text, button, leaveButton, badge, actions, message, card, heading, field, input, select, facts, copyField, clearSecrets, problem, date } from './dom.mjs';

const LIMITS = [
  ['rpm_limit', 'Requests per minute'], ['tpm_limit', 'Tokens per minute'], ['max_parallel_requests', 'Parallel requests'],
  ['spend_limit', 'Spend limit (USD)'], ['default_max_tokens', 'Output tokens reserved per request'],
];

export function editableValues(record, type) {
  const values = { name: record.name || '', reset_period: record.reset_period || '' };
  for (const [key] of LIMITS) values[key] = Number(record[key] || 0);
  if (type === 'keys') Object.assign(values, { description: record.description || '', role: record.role || 'user', expires_at: record.expires_at || '' });
  else values.allowed_models = record.allowed_models || [];
  return values;
}

export async function access(p, type = 'keys') {
  p.root.append(heading('Keys & teams', 'Give each person or client its own key. Shared teams set model access and shared limits.'), message('Loading access settings…'));
  const [keys, teams] = await Promise.all([p.api.list('/keys', { signal: p.scope.signal }), p.api.list('/teams', { signal: p.scope.signal })]);
  if (!p.current()) return;
  const editor = el('div');
  const lifetime = new Lifetime();
  p.scope.signal.addEventListener('abort', () => { lifetime.end(); clearSecrets(editor); }, { once: true });
  const listing = el('div');
  const records = type === 'keys' ? keys : teams;
  const showEditor = record => {
    if (!p.current() || !p.api.canLeave()) return;
    clearSecrets(editor);
    const scope = lifetime.next();
    const closeEditor = () => { if (!p.api.canLeave()) return; lifetime.end(); clearSecrets(editor); editor.replaceChildren(); };
    const owner = { ...p, current: () => p.current() && scope.current(), scope: { signal: AbortSignal.any([scope.signal, p.scope.signal]) }, closeEditor };
    editor.replaceChildren(accessForm(owner, type, record, teams, () => p.go('access', type)));
    p.syncActivity();
    editor.scrollIntoView({ behavior: 'instant', block: 'nearest' });
  };
  const tabs = actions(leaveButton('Keys', () => p.go('access', 'keys'), p.api), leaveButton('Teams', () => p.go('access', 'teams'), p.api));
  tabs.classList.add('tabs');
  tabs.children[type === 'keys' ? 0 : 1].setAttribute('aria-current', 'true');
  const body = el('tbody');
  for (const record of records) {
    const personal = type === 'teams' && record.kind === 'personal';
    body.append(el('tr', {},
      el('td', {}, el('strong', {}, record.name), text(record.id, 'muted')),
      el('td', {}, type === 'keys' ? record.role : record.kind),
      el('td', {}, badge(record.suspended ? 'Suspended' : record.is_expired ? 'Expired' : 'Active', record.suspended || record.is_expired ? 'warn' : 'good')),
      el('td', {}, leaveButton(personal ? 'View policy' : 'Manage', () => showEditor(record), p.api)),
    ));
  }
  listing.append(card(type === 'keys' ? 'Managed keys' : 'Teams', records.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Name / ID', 'Type', 'Status', ''].map(label => el('th', { scope: 'col' }, label)))), body)) : text(type === 'keys' ? 'No managed keys yet. Create one for each person or client.' : 'No teams yet. Create a shared team to control models and shared usage.', 'empty')));
  p.root.replaceChildren(heading('Keys & teams', 'Give each person or client its own key. Shared teams set model access and shared limits.', leaveButton(type === 'keys' ? 'Create key' : 'Create team', () => showEditor(null), p.api, 'primary')), tabs, editor, listing,
    text('Startup keys are managed in installation configuration. Team membership is fixed when a key is created; replace the key to change its team.', 'footer-note'));
}

function accessForm(p, type, record, teams, reload) {
  const isKey = type === 'keys';
  const personal = !isKey && record?.kind === 'personal';
  const own = record && p.identity.is_virtual && (isKey ? record.id === p.identity.id : record.id === p.identity.team_id);
  const panel = card(record ? `${personal ? 'View' : 'Manage'} ${record.name}` : isKey ? 'Create a named key' : 'Create a shared team');
  const feedback = el('div', { 'aria-live': 'polite' });
  if (personal) {
    panel.append(message('Personal teams are created with their owning key. Their policy cannot be edited independently; manage key limits from Keys.'), facts([['ID', record.id], ['Model access', (record.allowed_models || []).join(', ') || 'All models']]), leaveButton('Close', p.closeEditor, p.api));
    return panel;
  }
  const before = editableValues(record || {}, type);
  const form = el('form');
  const group = el('fieldset', { 'data-protected-actions': true });
  const fields = new Map();
  const add = (key, label, element, hint) => { fields.set(key, element); return field(label, element, hint); };
  const main = el('div', { class: 'form-grid' });
  main.append(add('id', 'ID', input('id', record?.id || '', { required: true, readOnly: !!record }), 'Stable identifier used in the API.'), add('name', 'Name', input('name', before.name, { required: true })));
  if (isKey) {
    main.append(add('description', 'Description', input('description', before.description)), add('role', 'Role', select('role', [['user', 'User: inference and models'], ['admin', 'Administrator: management']], before.role)));
    if (!record) {
      main.append(add('team_id', 'Team', select('team_id', [['', 'Create a personal team'], ...teams.filter(team => team.kind === 'shared' && !team.suspended).map(team => [team.id, team.name])], ''), 'Choose a shared team to restrict model access.'), add('team_role', 'Team role', select('team_role', [['member', 'Member'], ['owner', 'Owner']], 'member')));
    } else panel.append(facts([['Team', record.team_id], ['Team role', record.team_role], ['Expires', date(record.expires_at)]]));
    main.append(add('expires_at', 'Expiry (optional)', input('expires_at', '', { type: 'datetime-local' }), record?.expires_at ? 'Leave blank to keep the current expiry. Existing expiry cannot be cleared.' : 'Leave blank for no expiry.'));
  } else {
    group.append(add('allowed_models', 'Allowed models or model groups', el('textarea', { name: 'allowed_models', rows: '3' }, before.allowed_models.join('\n')), 'One exact model or group name per line. An empty list allows all models.'));
  }
  group.prepend(main);
  const limits = el('div', { class: 'form-grid' });
  for (const [key, label] of LIMITS) limits.append(add(key, label, input(key, before[key], { type: 'number', min: '0', step: key === 'spend_limit' ? 'any' : '1', required: true })));
  limits.append(add('reset_period', 'Spend reset period', select('reset_period', [['', 'Default'], ['daily', 'Daily'], ['weekly', 'Weekly'], ['monthly', 'Monthly']], before.reset_period)));
  group.append(el('h3', {}, 'Limits'), text('Zero means no limit at this scope. Key and team limits are both enforced. Output-token reservation is a budget estimate, not a model output cap.', 'muted'), limits);
  let blocked = false;
  const read = () => {
    const result = { name: fields.get('name').value.trim(), reset_period: fields.get('reset_period').value };
    for (const [key] of LIMITS) result[key] = Number(fields.get(key).value);
    if (isKey) {
      result.description = fields.get('description').value;
      result.role = fields.get('role').value;
      const expiry = fields.get('expires_at').value;
      result.expires_at = expiry ? new Date(expiry).toISOString() : before.expires_at;
      if (!record) {
        result.team_id = fields.get('team_id').value;
        if (result.team_id) result.team_role = fields.get('team_role').value;
      }
    } else result.allowed_models = [...new Set(fields.get('allowed_models').value.split('\n').map(value => value.trim()).filter(Boolean))];
    if (!record) { result.id = fields.get('id').value.trim(); if (!result.expires_at) delete result.expires_at; }
    return result;
  };
  const endpoint = record ? `/${type}/${encodeURIComponent(record.id)}` : `/${type}`;
  const mutate = async (method, body, suffix = '', refreshIdentity = false) => {
    if (!p.current() || blocked || group.disabled || !p.api.canChange()) return;
    const rotation = refreshIdentity && suffix === '/rotate' ? p.beginOwnRotation() : null;
    group.disabled = true;
    feedback.replaceChildren(message('Saving access change…'));
    try {
      const result = await p.api.data(endpoint + suffix, { method, body, signal: p.scope.signal, beforeReturn: async result => {
        if (!result.data?.key) return;
        feedback.replaceChildren(message('Change completed. Verifying browser access before showing the new key…'));
        try { await p.beforeKeyDisclosure(p.scope.signal); }
        catch (error) {
          if (rotation && error instanceof Obsolete) p.noteOwnRotationRecovery();
          throw error;
        }
      } });
      if (!p.current()) return;
      blocked = true;
      if (result?.key) {
        if (refreshIdentity) { p.showKeyAfterSession(result, rotation); return; }
        clearSecrets(panel);
        panel.replaceChildren(el('h2', {}, 'Save this key now'), message('This value is shown once. Give it only to the intended person or client. It disappears when you close this view, leave this page or refresh your session.', 'warning'), copyField('New key', result.key, true), facts([['Key ID', result.id], ['Role', result.role || record?.role]]), leaveButton('I saved the key', reload, p.api, 'primary'));
        p.scope.signal.addEventListener('abort', () => clearSecrets(panel), { once: true });
      } else if (refreshIdentity) await p.refresh();
      else feedback.replaceChildren(message('Change saved. Reload to see current state.'), leaveButton('Reload access', reload, p.api));
    } catch (error) {
      if (!p.current()) return;
      blocked = error.uncertain;
      if (blocked && refreshIdentity) {
        if (rotation) await p.recoverOwnRotation();
        else await p.refresh('The change outcome is unknown. Your access was rechecked; no write was retried.');
        return;
      }
      feedback.replaceChildren(problem(error));
      if (blocked) feedback.append(message(isKey && !record ? 'Reload the keys list. If the key was created but its value was lost, rotate it to issue a new value.' : 'Reload current state before another change.', 'warning'), leaveButton('Reload access', reload, p.api));
    } finally { if (rotation) p.endOwnRotation(rotation); if (p.current()) group.disabled = blocked; }
  };
  const save = async event => {
    event.preventDefault();
    if (!form.reportValidity() || !p.current() || group.disabled || !p.api.canChange()) return;
    const after = read();
    const patch = record ? changedFields(before, after) : after;
    if (!Object.keys(patch).length) { feedback.replaceChildren(message('No fields changed.')); return; }
    const willEndSession = own && isKey && patch.role !== undefined;
    if (willEndSession && !confirm('Changing the role of your current key ends this browser session. Continue?')) return;
    if (record) {
      group.disabled = true;
      try {
        const fresh = editableValues(await p.api.data(endpoint, { signal: p.scope.signal }), type);
        if (!p.current()) return;
        if (Object.keys(patch).some(key => JSON.stringify(fresh[key]) !== JSON.stringify(before[key]))) {
          blocked = true;
          feedback.replaceChildren(message('An edited field changed on the server. Reload before applying your changes.', 'warning'), leaveButton('Reload access', reload, p.api));
          return;
        }
      } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); return; }
      finally { if (p.current()) group.disabled = blocked; }
    }
    const refreshIdentity = own && isKey && (patch.role !== undefined || patch.expires_at !== undefined);
    await mutate(record ? 'PATCH' : 'POST', patch, '', refreshIdentity);
  };
  group.append(actions(el('button', { type: 'submit', class: 'primary' }, record ? 'Save changed fields' : isKey ? 'Create key' : 'Create team')));
  if (record) {
    group.append(el('hr'), actions(
      button(record.suspended ? 'Resume' : 'Suspend', async () => {
        if (!confirm(`${record.suspended ? 'Resume' : 'Suspend'} ${record.name}?${own ? ' This affects your current browser access.' : ''}`)) return;
        await mutate('PATCH', { suspended: !record.suspended }, '', own && !record.suspended);
      }),
      isKey ? button('Rotate key', async () => {
        if (!confirm(`Replace the secret for ${record.name}? Existing clients must use the new key.${own ? ' Your current session will end.' : ''}`)) return;
        await mutate('POST', undefined, '/rotate', own);
      }) : null,
      button(isKey ? 'Revoke key' : 'Delete team', async () => {
        if (!confirm(isKey ? `Permanently revoke ${record.name}? Its clients and MCP grants will lose access.${own ? ' This ends your current session.' : ''}` : `Delete ${record.name}? Shared teams must have no member keys.`)) return;
        await mutate('DELETE', undefined, '', own);
      }, 'danger'),
    ));
  }
  form.append(group);
  form.addEventListener('submit', event => { void save(event); });
  panel.append(form, feedback, actions(leaveButton('Close', p.closeEditor, p.api)), text('Only edited fields are sent. Membership is immutable. Reload this page if another administrator is changing the same settings.', 'footer-note'));
  return panel;
}
