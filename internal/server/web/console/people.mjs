import { sharingReadiness } from './sharing.mjs';
import { el, text, button, leaveButton, badge, actions, message, card, heading, input, select, field, facts, date, problem, copyField, clearSecrets, selectableRow, markSelection, editorActions, compactTable } from './dom.mjs';
import { details, formSubmission, readAfterWrite } from './details.mjs';
import { policySummary } from './access-policy.mjs';
import { passwordControl, validNewPassword } from './login.mjs';

const roleName = role => role === 'admin' ? 'Administrator' : 'User';
const invitationLabel = grant => grant.purpose === 'recovery' ? 'Password recovery' : 'New account';

function grantDisclosure(p, result, done) {
  clearSecrets(p.root);
  const fallback = `${p.state.issuer}/console/#${result.purpose === 'recovery' ? 'recover' : 'invite'}?token=${encodeURIComponent(result.token)}`;
  p.root.replaceChildren(el('h2', {}, result.purpose === 'recovery' ? 'Share the recovery link' : 'Share the invitation'),
    message('Copy this link now. It will not be shown again when you leave or refresh.', 'warning'),
    copyField('Invitation link', result.url || fallback, true), facts([['Expires', date(result.expires_at)], ['Team', result.team_id]]),
    text('Share privately with the intended person. The link is single-use and works only while they can reach this router.', 'muted'),
    leaveButton('Done', done, p.api, 'primary'));
}

export async function people(p, selection, mode) {
  const listing = el('div', { class: 'entity-list access-list' });
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  let records = [], teams = [], invitations = [], drawer;
  const search = input('people_search', '', { type: 'search', placeholder: 'Search people', 'aria-label': 'Search people' });
  const results = el('div'), inviteRows = el('div');
  const teamName = id => teams.find(team => team.id === id)?.name || id || 'Unavailable';
  const read = async () => {
    const loaded = await Promise.all(['/access/people', '/teams', '/access/invitations'].map(path => p.api.list(path, { signal: p.scope.signal })));
    if (!p.current()) return;
    [records, teams, invitations] = loaded;
    render();
  };
  const open = async (next, nextMode) => {
    drawer?.dispose(!next && nextMode !== 'create'); drawer = undefined;
    selection = next; mode = nextMode; markSelection(listing, next);
    if (!next && nextMode !== 'create') return;
    const owner = details({ ...p, root: workspace }, mode === 'create' ? 'Invite person' : next?.startsWith('invitation/') ? 'Invitation details' : 'Person details', () => p.go('people'));
    drawer = owner;
    const reload = async (notice = 'Changes saved.') => {
      const revision = owner.panel.revision;
      await read();
      if (!owner.panel.current() || owner.panel.revision !== revision || !p.api.canLeave()) return;
      await open(selection, mode);
      if (drawer?.current()) drawer.root.prepend(message(notice));
    };
    if (mode === 'create') {
      inviteForm(owner, teams, result => grantDisclosure(owner, result, () => p.reload()));
      return;
    }
    if (next.startsWith('invitation/')) {
      const grant = invitations.find(entry => entry.id === next.slice(11));
      if (!grant) { owner.root.append(message('This invitation is no longer available.', 'warning')); return; }
      owner.root.append(el('h2', {}, invitationLabel(grant)), badge(grant.status), facts([['Team', teamName(grant.team_id)], ['Role', roleName(grant.role)], ['Created', date(grant.created_at)], ['Expires', date(grant.expires_at)]]));
      if (grant.status === 'pending') {
        const feedback = el('div');
        const revoke = button('Revoke invitation', async () => {
          if (!p.api.canChange() || revoke.disabled || !confirm('Revoke this invitation? It will no longer allow someone to join or reset a password.')) return;
          revoke.disabled = true;
          try {
            await p.api.data(`/access/invitations/${encodeURIComponent(grant.id)}`, { method: 'DELETE', signal: owner.scope.signal });
            if (owner.current()) await readAfterWrite(owner, () => reload('Invitation revoked.'));
          } catch (error) { if (owner.current()) feedback.replaceChildren(problem(error), leaveButton('Reload invitation', () => reload('Current invitation loaded.'), p.api)); }
        }, 'danger');
        revoke.setAttribute('data-protected-actions', ''); owner.root.append(revoke, feedback);
      }
      p.syncActivity(); return;
    }
    owner.root.append(message('Loading person…'));
    try {
      const record = await p.api.data(`/access/people/${encodeURIComponent(next)}`, { signal: owner.scope.signal });
      if (!owner.current()) return;
      const show = () => {
        const view = owner.panel.view('Person details');
        const team = teams.find(team => team.id === record.team_id);
        view.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, record.name), leaveButton('Edit', edit, p.api, 'primary')),
          badge(record.suspended ? 'Suspended' : 'Active', record.suspended ? 'warn' : 'good'), ...(record.owner ? [badge('Owner')] : []),
          facts([['Username', record.username], ['Role', roleName(record.role)], ['Team', teamName(record.team_id)], ['Created', date(record.created_at)]]),
          team ? el('section', { class: 'access-section' }, el('h3', {}, 'Inherited team access'), policySummary(team), leaveButton('View team', () => p.go('access', `teams/${record.team_id}`), p.api, 'link')) : message('Team policy is unavailable.', 'warning'));
        if (record.owner) view.root.append(text('The owner and its team stay active. Use local owner recovery if the password is lost.', 'muted'));
        else {
          const feedback = el('div');
          const recovery = button('Create recovery link', async () => {
            if (recovery.disabled || !p.api.canChange() || !confirm(`Create a password recovery link for ${record.name}? Share it only with this person.`)) return;
            recovery.disabled = true;
            try {
              const result = await p.api.data(`/access/people/${encodeURIComponent(record.id)}/recovery`, { method: 'POST', body: {}, signal: view.scope.signal, beforeReturn: () => p.beforeKeyDisclosure?.(view.scope.signal) });
              if (view.current()) grantDisclosure(view, result, () => p.reload());
            } catch (error) { if (view.current()) { feedback.replaceChildren(problem(error)); recovery.disabled = !!error.uncertain; } }
          });
          recovery.setAttribute('data-protected-actions', ''); view.root.append(el('section', { class: 'access-section' }, el('h3', {}, 'Account recovery'), text('Create a single-use link so this person can choose a new password.'), recovery, feedback));
        }
        p.syncActivity();
      };
      const edit = () => personForm(owner.panel.view('Edit person'), record, teams, show, () => reload());
      show();
    } catch (error) { if (owner.current()) owner.root.replaceChildren(problem(error), leaveButton('Reload person', () => open(next), p.api)); }
  };
  const render = () => {
    const query = search.value.trim().toLowerCase();
    const shown = records.filter(person => `${person.name} ${person.username} ${teamName(person.team_id)}`.toLowerCase().includes(query));
    results.replaceChildren(shown.length ? compactTable(['Person', 'Team', 'Role', 'Status'],
      el('tbody', {}, shown.map(person => selectableRow(person.id, selection, () => p.go('people', person.id),
        el('td', {}, leaveButton(person.name, () => p.go('people', person.id), p.api, 'link entity-title'), text(person.username, 'muted')),
        el('td', {}, teamName(person.team_id)), el('td', {}, person.owner ? 'Owner' : roleName(person.role)), el('td', {}, badge(person.suspended ? 'Suspended' : 'Active', person.suspended ? 'warn' : 'good')))))) : text(query ? 'No matching people.' : 'No local accounts yet. Run zzrouter console on the installed computer to create the owner.', 'empty'));
    inviteRows.replaceChildren(invitations.length ? el('ul', { class: 'invitation-list' }, invitations.map(grant => el('li', { 'data-selection': `invitation/${grant.id}` },
      leaveButton(`${invitationLabel(grant)} · ${teamName(grant.team_id)}`, () => p.go('people', `invitation/${grant.id}`), p.api, 'link'), badge(grant.status), text(`Expires ${date(grant.expires_at)}`, 'muted')))) : text('No invitations. Invite a person to give them their own account.', 'empty'));
    markSelection(listing, selection);
  };
  search.addEventListener('input', render);
  listing.append(card('Accounts', search, results), card('Invitations', inviteRows));
  p.root.append(heading('People', 'Local accounts for people. Shared teams define their access.', leaveButton('Refresh', () => p.reload(), p.api), leaveButton('+ Invite person', () => p.go('people', undefined, 'create'), p.api, 'primary')), workspace);
  p.handleSelection?.(open);
  await read();
  if (p.current()) await open(selection, mode);
}

function inviteForm(p, teams, saved) {
  const available = teams.filter(team => team.kind === 'shared' && !team.suspended);
  if (!available.length) { p.root.append(message('Create an active shared team before inviting someone.'), leaveButton('Create team', () => p.go('access', 'teams', 'create'), p.api, 'primary')); return; }
  const form = el('form', { class: 'entity-editor' });
  const draft = p.trackDraft(form);
  const team = select('team_id', available.map(row => [row.id, row.name]), available[0].id);
  const role = select('role', [['user', 'User: Chat and model access'], ['admin', 'Administrator: manage this router']], 'user');
  const expiry = select('expires_in_hours', [['24', '24 hours'], ['72', '3 days'], ['168', '7 days']], '24');
  const preview = el('div', { class: 'policy-preview' });
  const showPolicy = () => preview.replaceChildren(el('h3', {}, 'Inherited team access'), policySummary(available.find(row => row.id === team.value)));
  team.addEventListener('change', showPolicy); showPolicy();
  const feedback = el('div', { 'aria-live': 'polite' });
  const controls = el('fieldset', { 'data-protected-actions': true }, field('Team', team), field('Role', role, 'Administrators can manage providers, people, keys and policies.'), field('Invitation expires in', expiry), preview);
  const submit = el('button', { type: 'submit', class: 'primary' }, 'Create invitation');
  const submission = formSubmission(p, controls, submit, draft);
  form.append(feedback, controls, editorActions(submit, p.close, p.api, draft));
  form.addEventListener('submit', async event => {
    event.preventDefault(); if (!form.reportValidity()) return;
    await submission.run(() => p.api.data('/access/invitations', { method: 'POST', body: { team_id: team.value, role: role.value, expires_in_hours: Number(expiry.value) }, signal: p.scope.signal, beforeReturn: () => p.beforeKeyDisclosure?.(p.scope.signal) }), saved,
      error => feedback.replaceChildren(problem(error), leaveButton('Review invitations', () => p.reload(), p.api)));
  });
  p.root.append(sharingReadiness(p), text('No email service is needed. Copy the invitation link and share it privately.', 'muted'), form);
  p.syncActivity();
}

function personForm(p, record, teams, cancel, saved) {
  const form = el('form', { class: 'entity-editor' });
  const draft = p.trackDraft(form);
  const name = input('name', record.name, { required: true, autocomplete: 'name', maxlength: '100' });
  const role = select('role', [['user', 'User'], ['admin', 'Administrator']], record.role);
  const team = select('team_id', teams.filter(row => row.kind === 'shared').map(row => [row.id, row.name]), record.team_id);
  const status = select('suspended', [['false', 'Active'], ['true', 'Suspended']], String(record.suspended));
  role.disabled = team.disabled = status.disabled = record.owner;
  const controls = el('fieldset', { 'data-protected-actions': true }, field('Name', name), field('Team', team), field('Role', role), field('Status', status), record.owner ? text('The owner’s role, team and active status are protected.', 'muted') : text('Changes to access or status end this person’s existing sessions.', 'muted'));
  const feedback = el('div', { 'aria-live': 'polite' });
  const save = el('button', { type: 'submit', class: 'primary' }, 'Save changes');
  const submission = formSubmission(p, controls, save, draft);
  form.append(feedback, controls, editorActions(save, cancel, p.api, draft));
  form.addEventListener('submit', async event => {
    event.preventDefault(); if (!form.reportValidity()) return;
    const body = { name: name.value, ...(!record.owner ? { role: role.value, team_id: team.value, suspended: status.value === 'true' } : {}) };
    await submission.run(() => p.api.data(`/access/people/${encodeURIComponent(record.id)}`, { method: 'PATCH', body, signal: p.scope.signal }),
      () => record.id === p.identity.id ? p.refresh('Your account changed.') : readAfterWrite(p, saved),
      error => feedback.replaceChildren(problem(error), leaveButton('Reload person', () => p.reload(), p.api)));
  });
  p.root.append(form); name.focus({ preventScroll: true }); p.syncActivity();
}

export function accountSettings(p) {
  const current = passwordControl('current_password', 'current-password');
  const next = passwordControl('password');
  const feedback = el('div', { 'aria-live': 'polite' });
  const controls = el('fieldset', { 'data-protected-actions': true }, current.field('Current password'), next.field('New password'), text('Use 12 to 128 characters. Changing your password signs out your other sessions.', 'field-hint'));
  const save = el('button', { type: 'submit', class: 'primary' }, 'Change password');
  const form = el('form', {}, controls, save, feedback);
  const submission = formSubmission(p, controls, save);
  form.addEventListener('submit', async event => {
    event.preventDefault(); if (!form.reportValidity()) return;
    await submission.run(async () => {
      if (!validNewPassword(next.control.value)) throw new Error('Use a password with 12 to 128 characters.');
      const body = { current_password: current.control.value, password: next.control.value };
      clearSecrets(form);
      return p.api.data('/access/password', { method: 'POST', body, signal: p.scope.signal, notifyAuth: false });
    }, () => p.refresh('Password changed. Other sessions have been signed out.'), (error, blocked) => {
      feedback.replaceChildren(problem(error));
      if (blocked) feedback.append(message('Your password may have changed. Sign in again to verify it.'), leaveButton('Refresh sign-in', () => p.refresh(), p.api));
    });
  });
  const revokeFeedback = el('div', { 'aria-live': 'polite' });
  const revoke = button('Sign out other sessions', async () => {
    if (revoke.disabled || !p.api.canChange() || !confirm('Sign out your other sessions and revoke authorizations for this account?')) return;
    revoke.disabled = true;
    try {
      await p.api.data('/access/sessions/revoke', { method: 'POST', body: {}, signal: p.scope.signal, notifyAuth: false });
      if (p.current()) await p.refresh('Other sessions and authorizations were revoked.');
    } catch (error) { if (p.current()) revokeFeedback.replaceChildren(problem(error), leaveButton('Refresh account', () => p.refresh(), p.api)); }
  });
  revoke.setAttribute('data-protected-actions', '');
  const panel = card('Account security', el('details', {}, el('summary', {}, 'Change password'), form), el('section', { class: 'access-section' }, revoke, revokeFeedback));
  if (p.identity.owner) panel.append(leaveButton('Resume setup', async () => {
    try {
      const progress = await p.api.data('/access/onboarding', { signal: p.scope.signal });
      if (!p.current()) return;
      if (progress.response_verified) { await p.go('settings', 'setup'); return; }
      await p.api.data('/access/onboarding', { method: 'PATCH', body: { dismissed: false }, signal: p.scope.signal });
      if (p.current()) await p.go('overview');
    } catch (error) { if (p.current()) revokeFeedback.replaceChildren(problem(error)); }
  }, p.api));
  p.scope.signal.addEventListener('abort', () => clearSecrets(panel), { once: true });
  return panel;
}
