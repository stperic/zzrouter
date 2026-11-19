import { readAfterWrite } from './details.mjs';
import { changedFields, Obsolete } from './api.mjs';
import { el, text, button, leaveButton, actions, message, field, input, select, facts, copyField, clearSecrets, date, editorActions, formFeedback, invalidField } from './dom.mjs';
import { providerPolicy, routePolicy, routeFields, providerFields, modelFields, policySummary } from './access-policy.mjs';
import { budgetFields, budgetUsage } from './access-budget.mjs';

const LIMITS = [
  ['rpm_limit', 'Requests per minute'], ['tpm_limit', 'Tokens per minute'],
  ['max_parallel_requests', 'Parallel requests'], ['default_max_tokens', 'Output tokens reserved per request'],
];

export function editableValues(record, type) {
  const values = { name: record.name || '' };
  if (type === 'keys') Object.assign(values, { description: record.description || '', role: record.role || 'user', expires_at: record.expires_at || '' });
  else {
    Object.assign(values, { reset_period: record.reset_period || 'monthly', spend_limit: Number(record.spend_limit || 0), route_access: routePolicy(record), provider_access: providerPolicy(record), allowed_models: [...(record.allowed_models || [])].sort() });
    for (const [key] of LIMITS) values[key] = Number(record[key] || 0);
  }
  return values;
}

export const limitSummary = record => facts(LIMITS.map(([key, label]) => [label, Number(record[key]) > 0 ? Number(record[key]).toLocaleString() : key === 'default_max_tokens' ? 'Automatic estimate' : 'No limit']));

export function membershipSummary(record) {
  const m = record?.membership;
  if (!m) return 'Membership unavailable';
  const count = (value, single, plural = single + 's') => `${value} ${value === 1 ? single : plural}`;
  return [count(m.people, 'person', 'people'), count(m.app_keys, 'app key'), count(m.invitations, 'pending invitation'),
    m.enrollment_grants ? count(m.enrollment_grants, 'other pending enrollment') : null].filter(Boolean).join(' · ');
}

export function accessForm(p, type, record, options, reload) {
  const { catalog = [], route } = options;
  const isKey = type === 'keys';
  const personal = !isKey && record?.kind === 'personal';
  const routeOnly = route && record;
  const own = record && p.identity.is_virtual && (isKey ? record.id === p.identity.id : record.id === p.identity.team_id);
  const panel = el('section', { class: 'access-editor' }, el('h2', {}, record ? record.name : isKey ? 'Create a named key' : 'Create a shared team'));
  const feedback = el('div', { 'aria-live': 'polite' });
  if (personal) {
    panel.append(message('This personal team belongs to one existing key. To manage a provider policy and shared limits, create a replacement key assigned to a shared team.'), policySummary(record, catalog), facts([['ID', record.id]]));
    return { root: panel };
  }
  const before = editableValues(record || {}, type);
  const form = el('form', { class: 'entity-editor' });
  const draft = p.trackDraft?.(form);
  const group = el('fieldset', { 'data-protected-actions': true });
  const submit = el('button', { type: 'submit', class: 'primary', 'data-protected-actions': true }, routeOnly ? 'Save route access' : record ? 'Save changes' : isKey ? 'Create key' : 'Create team');
  const setDisabled = disabled => { group.disabled = disabled; submit.disabled = disabled; };
  const fields = new Map();
  const validation = formFeedback(form, feedback, key => fields.get(key) || fields.get(String(key).split(/[.[]/, 1)[0]));
  let controls;
  if (routeOnly) {
    const routes = routeFields(record, options.groups, false, route);
    fields.set('route_access', routes.control);
    group.append(routes.root);
    controls = { read: () => ({ route_access: routes.read() }), focus: () => routes.control.focus() };
  } else controls = accessFields(p, type, record, options, before, group, fields);
  const { read, ...formActions } = controls;
  let blocked = false;
  const endpoint = record ? `/${type}/${encodeURIComponent(record.id)}` : `/${type}`;
  const mutate = async (method, body, suffix = '', refreshIdentity = false) => {
    if (!p.current() || blocked || group.disabled || !p.api.canChange()) return;
    const rotation = refreshIdentity && suffix === '/rotate' ? p.beginOwnRotation() : null;
    setDisabled(true);
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
      draft?.clean();
      if (result?.key) {
        if (refreshIdentity) { p.showKeyAfterSession(result, rotation); return; }
        clearSecrets(panel);
        panel.replaceChildren(el('h2', {}, 'Save this key now'), message('This value is shown once. Give it only to the intended person or client. It disappears when you close this view, leave this page or refresh your session.', 'warning'), copyField('New key', result.key, true), facts([['Key ID', result.id], ['Role', result.role || record?.role]]), leaveButton('I saved the key', () => readAfterWrite(p, () => reload(result.id || record?.id), 'Key saved.'), p.api, 'primary'));
        p.scope.signal.addEventListener('abort', () => clearSecrets(panel), { once: true });
      } else if (refreshIdentity) await p.refresh();
      else if (!record && !isKey && options.created) options.created(result);
      else await readAfterWrite(p, () => reload(record?.id || result?.id, method === 'DELETE'), 'Changes saved.');
    } catch (error) {
      if (!p.current()) return;
      blocked = error.uncertain;
      if (blocked && refreshIdentity) {
        if (rotation) await p.recoverOwnRotation();
        else await p.refresh('The change outcome is unknown. Your access was rechecked; no write was retried.');
        return;
      }
      validation.error(error);
      if (blocked) feedback.append(message(isKey && !record ? 'Reload the keys list. If the key was created but its value was lost, rotate it to issue a new value.' : 'Reload current state before another change.', 'warning'), leaveButton('Reload access', reload, p.api));
    } finally { if (rotation) p.endOwnRotation(rotation); if (p.current()) setDisabled(blocked); }
  };
  const save = async event => {
    event.preventDefault();
    if (!form.reportValidity() || !p.current() || group.disabled || !p.api.canChange()) return;
    let after;
    validation.clear();
    try { after = read(); } catch (error) { validation.error(error); return; }
    const patch = record ? changedFields(before, after) : after;
    if (!Object.keys(patch).length) { feedback.replaceChildren(message('No fields changed.')); return; }
    const willEndSession = own && isKey && patch.role !== undefined;
    if (willEndSession && !confirm('Changing the role of your current key ends this browser session. Continue?')) return;
    if (record) {
      setDisabled(true);
      try {
        const fresh = editableValues(await p.api.data(endpoint, { signal: p.scope.signal }), type);
        if (!p.current()) return;
        if (Object.keys(patch).some(key => JSON.stringify(fresh[key]) !== JSON.stringify(before[key]))) {
          blocked = true;
          feedback.replaceChildren(message('An edited field changed on the server. Reload before applying your changes.', 'warning'), leaveButton('Reload access', reload, p.api));
          return;
        }
      } catch (error) { if (p.current()) validation.error(error); return; }
      finally { if (p.current()) setDisabled(blocked); }
    }
    const refreshIdentity = own && (isKey ? patch.role !== undefined || patch.expires_at !== undefined : patch.route_access !== undefined || patch.provider_access !== undefined || patch.allowed_models !== undefined);
    await mutate(record ? 'PATCH' : 'POST', patch, '', refreshIdentity);
  };
  if (record && !routeOnly) {
    group.append(el('section', { class: 'access-section danger-zone' }, el('h3', {}, 'Lifecycle'), text(isKey ? 'Rotating or revoking a key affects every client using it.' : `${membershipSummary(record)}. Remove or reassign people, keys and pending enrollments before deletion.`, 'muted'), actions(
      (isKey || record.suspended || record.membership?.can_suspend) ? button(record.suspended ? 'Resume' : 'Suspend', async () => {
        if (!confirm(`${record.suspended ? 'Resume' : 'Suspend'} ${record.name}?${own ? ' This affects your current browser access.' : ''}`)) return;
        await mutate('PATCH', { suspended: !record.suspended }, '', own && !record.suspended);
      }) : text(record.membership?.owner_protected ? 'The owner’s team cannot be suspended.' : 'Reload membership to check lifecycle actions.', 'muted'),
      isKey ? button('Rotate key', async () => {
        if (!confirm(`Replace the secret for ${record.name}? Existing clients must use the new key.${own ? ' Your current session will end.' : ''}`)) return;
        await mutate('POST', undefined, '/rotate', own);
      }) : null,
      (isKey || record.membership?.can_delete) ? button(isKey ? 'Revoke key' : 'Delete team', async () => {
        if (!confirm(isKey ? `Permanently revoke ${record.name}? Its clients and MCP grants will lose access.${own ? ' This ends your current session.' : ''}` : `Delete ${record.name}? The team must have no people, app keys or pending enrollments.`)) return;
        await mutate('DELETE', undefined, '', own);
      }, 'danger') : null,
    )));
  }
  const footer = editorActions(submit, p.closeEditor, p.api, draft);
  form.append(feedback, group, footer);
  form.addEventListener('submit', event => { void save(event); });
  panel.append(form);
  return { root: panel, draft, ...formActions };
}

function accessFields(p, type, record, options, before, group, fields) {
  const { teams, catalog = [], models = [], groups = [] } = options;
  const readOwner = options.readOwner || p;
  const isKey = type === 'keys';
  const add = (key, label, element, hint) => { fields.set(key, element); return field(label, element, hint); };
  const name = input('name', before.name, { required: true, placeholder: isKey ? 'e.g. Code editor' : 'e.g. Engineering' });
  const id = input('id', record?.id || '', { required: true, readOnly: !!record });
  let customID = !!record;
  id.addEventListener('input', () => { customID = true; });
  name.addEventListener('input', () => { if (!customID) id.value = name.value.normalize('NFKD').replace(/[\u0300-\u036f]/g, '').toLowerCase().trim().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, ''); if (name.value.trim() && !id.value) identity.setAttribute('open', ''); });
  group.append(add('name', 'Name', name, isKey ? 'Shown to clients and in the network overview.' : 'A shared policy and budget for people and applications.'));
  const identity = el('details', { class: 'access-section' }, el('summary', {}, 'Identifier & description'), add('id', 'ID', id, 'Generated from the name. Enter an ID if the name uses no Latin letters or digits. Stable after creation.'));
  id.addEventListener('invalid', () => identity.setAttribute('open', ''));
  const inherited = el('div', { class: 'inherited-policy' });
  const providers = isKey ? null : providerFields(before, catalog, options.catalogUnavailable);
  const teamRole = add('team_role', 'Team role', select('team_role', [['member', 'Member'], ['owner', 'Owner']], 'member'));
  const showInherited = () => {
    const value = record?.team_id || fields.get('team_id')?.value;
    const team = teams.find(team => team.id === value);
    teamRole.hidden = !value;
    inherited.replaceChildren();
    if (team) {
      inherited.append(el('h3', {}, `${team.name} policy`), policySummary(team, catalog), budgetUsage(readOwner, 'teams', team, { compact: true }), el('details', {}, el('summary', {}, 'Usage limits'), limitSummary(team)), text('Managed in Teams. Policy changes apply to every key in this team.', 'muted'));
      if (team.kind === 'personal') inherited.append(message('This key uses a personal team. Create a replacement key assigned to a shared team to configure provider access and limits.'));
    }
    else inherited.append(text(value ? 'Team policy unavailable. Reload before creating a key.' : 'Choose a team to preview its access and shared budget.', 'muted'));
  };
  if (isKey) {
    identity.append(add('description', 'Description', input('description', before.description)));
    if (!record) {
      const choice = select('team_id', [['', 'Choose a team…'], ...teams.filter(team => team.kind === 'shared' && !team.suspended).map(team => [team.id, team.name])], '');
      choice.required = true;
      choice.addEventListener('change', showInherited);
      group.append(el('section', { class: 'access-section' }, el('h3', {}, 'Team & access'), add('team_id', 'Team', choice, 'Provider access, budgets and usage limits are managed by the team.'), button('+ Create shared team', () => options.createTeam(), 'link'), inherited));
    } else group.append(el('section', { class: 'access-section' }, el('h3', {}, 'Inherited team access'), inherited, facts([['Team role', record.team_role || 'Member']]), text('Team membership is fixed. Create a replacement key to change its team.', 'muted')));
    showInherited();
  }
  const routes = isKey ? null : routeFields(record, groups, options.modelsUnavailable, options.route);
  const modelAccess = isKey ? null : modelFields(before, models, groups, options.modelsUnavailable);
  const budget = isKey ? null : budgetFields(before);
  if (!isKey) {
    fields.set('route_access', routes.control); fields.set('provider_access', providers.control); fields.set('allowed_models', modelAccess.control);
    for (const [key, control] of Object.entries(budget.fields)) fields.set(key, control);
    group.append(el('section', { class: 'access-group' }, el('h3', {}, 'Access'),
      text(`${record ? membershipSummary(record) + '. ' : ''}Route, provider and model permissions all apply.`, 'muted'), routes.root, providers.root,
      el('details', { class: 'access-section', open: before.allowed_models.length > 0 }, el('summary', {}, 'Restrict specific models'), modelAccess.root)), budget.root);
  }
  if (isKey) group.append(el('section', { class: 'access-section' }, add('expires_at', 'Expiry (optional)', input('expires_at', '', { type: 'datetime-local' }), record?.expires_at ? `Current: ${date(record.expires_at)}. Leave blank to keep it. Existing expiry cannot be cleared.` : 'Leave blank for no expiry.')));
  const advanced = el('details', { class: 'access-section' }, el('summary', {}, isKey ? 'Advanced permissions' : 'Advanced usage limits'));
  if (isKey) {
    advanced.append(add('role', 'Key role', select('role', [['user', 'User: inference and models'], ['admin', 'Administrator: management']], before.role)), text('An administrator can change access policies. Use User for clients.', 'muted'));
    if (!record) advanced.append(teamRole);
  } else {
    const limits = el('div', { class: 'form-grid access-limits' });
    for (const [key, label] of LIMITS) limits.append(add(key, label, input(key, before[key], { type: 'number', min: '0', step: '1', required: true })));
    advanced.append(text('Zero means no limit. Output-token reservation estimates budget usage; it does not cap model output.', 'muted'), limits);
  }
  group.append(identity, advanced);
  if (isKey && record && (record.spend_limit > 0 || LIMITS.some(([key]) => record[key] > 0))) {
    group.append(el('section', { class: 'access-section' }, el('h3', {}, 'Additional key limits'), text('These existing settings still apply alongside the team policy. They are read-only here. New console keys use team limits.', 'muted'), budgetUsage(readOwner, 'keys', record), limitSummary(record)));
  }
  const read = () => {
    const result = { name: fields.get('name').value.trim() };
    if (isKey) {
      result.description = fields.get('description').value;
      result.role = fields.get('role').value;
      const expiry = fields.get('expires_at').value;
      result.expires_at = expiry ? new Date(expiry).toISOString() : before.expires_at;
      if (!record) {
        const team = fields.get('team_id').value;
        if (!teams.some(item => item.id === team && item.kind === 'shared' && !item.suspended)) throw invalidField('Choose a shared team, or create one for this key.', fields.get('team_id'));
        result.team_id = team; result.team_role = fields.get('team_role').value;
      }
    } else {
      Object.assign(result, { route_access: routes.read(), provider_access: providers.read(), allowed_models: modelAccess.read(), ...budget.read() });
      for (const [key] of LIMITS) result[key] = Number(fields.get(key).value);
    }
    if (!record) { result.id = fields.get('id').value.trim(); if (!result.expires_at) delete result.expires_at; }
    return result;
  };
  return {
    read, focus: () => name.focus(), focusTeam: () => fields.get('team_id')?.focus(),
    selectTeam: team => { teams.push(team); fields.get('team_id').append(el('option', { value: team.id }, team.name)); fields.get('team_id').value = team.id; showInherited(); },
    refreshTeams: fresh => {
      teams.splice(0, teams.length, ...fresh);
      const choice = fields.get('team_id');
      const value = choice.value;
      choice.replaceChildren(...select('team_id', [['', 'Choose a team…'], ...teams.filter(team => team.kind === 'shared' && !team.suspended).map(team => [team.id, team.name])], value).children);
      choice.value = teams.some(team => team.id === value && team.kind === 'shared' && !team.suspended) ? value : '';
      showInherited();
    },
  };
}
