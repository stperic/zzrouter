import { el, text, button, leaveButton, actions, field, input, facts, message, problem, date, editorActions, formFeedback, selectableRow, markSelection } from './dom.mjs';
import { details, readAfterWrite } from './details.mjs';
import { liveReads } from './live-reads.mjs';
import { workloadKey } from './workload-data.mjs';

export function versionText(value) {
  if (typeof value === 'string') return value;
  if (!value || ![value.major, value.minor, value.patch].every(Number.isInteger)) return 'Not reported';
  return `${value.major}.${value.minor}.${value.patch}${value.pre_release ? '-' + value.pre_release : ''}${value.build_meta ? '+' + value.build_meta : ''}`;
}

function statusDetails(value = {}) {
  const operation = value.operation || value.privileged_run;
  return el('div', {}, facts([['Installed version', versionText(value.current_version)], ['Latest reported', versionText(value.latest_version)],
    ['State', value.state || 'Not reported'], ['Channel', value.channel], ['Last checked', value.last_check_time ? date(value.last_check_time) : 'Not reported'],
    ['Update source', value.source], ['Delegated to', value.delegated_to || 'This node'], ['Progress', value.download_progress === undefined ? 'Not reported' : `${value.download_progress}%`]]),
    value.blocked ? message(value.blocked, 'warning') : null, value.error ? message(value.error, 'error') : null,
    value.confirmation_error ? message(value.confirmation_error, 'warning') : null,
    value.restart_required ? message(value.restart_required_reason || 'A local restart is required to finish this update.', 'warning') : null,
    value.pending_confirm ? message(`Version ${value.pending_confirm.to_version} is awaiting confirmation.`, 'warning') : null,
    operation ? el('section', { class: 'access-section' }, el('h3', {}, 'Latest operation'), facts([['Action', operation.action], ['State', operation.state], ['Phase', operation.phase], ['From', operation.from_version], ['Target', operation.to_version], ['Finished', operation.finished_at ? date(operation.finished_at) : 'Not reported']]), operation.error ? message(operation.error, 'error') : null) : null);
}

export async function updates(p, selected, mode) {
  let rows = [], rollouts = [], drawer, selectionVersion = 0;
  const listing = el('section', { class: 'card entity-list' }), results = el('div'), feedback = el('div', { 'aria-live': 'polite' });
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  const listURL = () => p.go('settings', 'updates');
  const open = name => p.go('settings', 'updates/' + encodeURIComponent(name));
  const refresh = leaveButton('Refresh status', () => live.refresh(), p.api);
  const check = button('Check for updates', async () => {
    if (!p.api.canChange()) return;
    check.disabled = true; feedback.replaceChildren(message('Checking the coordinator’s release feed…'));
    try {
      const value = await p.api.data('/update/check', { method: 'POST', signal: p.scope.signal, timeoutMs: 60000 });
      if (!p.current()) return;
      await live.refresh();
      if (p.current()) feedback.prepend(message(value.skipped_reason || (value.update_available ? 'An update is available in the coordinator’s release feed.' : 'No newer release is reported by the coordinator.')));
    } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
    finally { if (p.current()) check.disabled = false; }
  }); check.setAttribute('data-protected-actions', '');
  listing.append(el('h2', {}, 'Software updates'), text('Review node versions and choose an explicit release and target machines.', 'muted'),
    actions(refresh, check, leaveButton('Install version', () => p.go('settings', 'updates', 'create'), p.api, 'primary')), feedback, results);
  p.root.append(workspace);
  const render = () => {
    const body = el('tbody');
    for (const row of rows) body.append(selectableRow(row.node, selected, () => open(row.node),
      el('td', {}, leaveButton(row.node, () => open(row.node), p.api, 'link entity-title')),
      el('td', {}, row.status ? versionText(row.status.current_version) : 'Unavailable'),
      el('td', {}, row.error || row.status?.state || 'Not reported')));
    results.replaceChildren(rows.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Node', 'Installed', 'Observation'].map(label => el('th', {}, label)))), body)) : text('No update observations are available.', 'empty'),
      ...rollouts.slice(0, 10).map(record => el('details', { class: 'access-section' }, el('summary', {}, `${record.version} · ${record.state}`),
        ...(record.nodes || []).map(node => text(`${node.node}: ${node.state}${node.error ? ' · ' + node.error : ''}`)),
        record.unchecked_peers?.length ? message(`Compatibility was not observed for: ${record.unchecked_peers.join(', ')}`, 'warning') : null,
        record.job_id ? leaveButton('View update job', () => p.go('workloads', workloadKey('job', p.state.node_name, record.job_id), 'jobs'), p.api) : null)));
  };
  const live = liveReads(p, async automatic => {
    const result = await p.api.data('/update/status?nodes=all', { signal: p.scope.signal, timeoutMs: 60000 });
    if (!p.current() || automatic && !live.canUpdate()) return false;
    rows = result.nodes || []; rollouts = result.rollouts || []; render(); feedback.replaceChildren(); return true;
  }, { eligible: () => !drawer && !listing.contains(document.activeElement), failed: error => feedback.replaceChildren(message('The latest status could not be read. Any displayed observations may be out of date.', 'warning'), problem(error)) });
  async function choose(name, nextMode) {
    const version = ++selectionVersion;
    drawer?.dispose(!name && nextMode !== 'create'); drawer = undefined; selected = name;
    markSelection(listing, name);
    if (!name && nextMode !== 'create') return;
    const owner = details({ ...p, root: workspace }, nextMode === 'create' ? 'Install software version' : 'Node software', listURL);
    drawer = owner;
    if (nextMode === 'create') { updateForm(owner); return; }
    await nodeDetails(owner, name, version);
  }
  async function nodeDetails(owner, name, version, notice) {
    const reader = owner.panel.view('Node software');
    const body = el('div'), note = el('div');
    const schedule = leaveButton('Edit update schedule', () => scheduleForm(owner, name), p.api);
    const rollback = button('Roll back coordinator', async () => {
      if (!p.api.canChange() || !confirm(`Restore the previous coordinator version on ${name}? A restart may interrupt the console.`)) return;
      rollback.disabled = true;
      try {
        const value = await p.api.data('/update/rollback', { method: 'POST', signal: reader.scope.signal, timeoutMs: 120000 });
        if (!reader.current()) return;
        await resultView(owner, value, 'Rollback request returned. Verify the coordinator version and any required local restart.');
      } catch (error) { if (reader.current()) await resultView(owner, null, '', error); }
    }, 'danger'); rollback.setAttribute('data-protected-actions', '');
    reader.root.append(el('h2', {}, name), notice ? message(notice) : '',
      actions(leaveButton('Refresh', () => watch.refresh(), p.api), schedule, name === p.state.node_name ? rollback : null), note, body);
    const watch = liveReads(reader, async automatic => {
      const [observed, history] = await Promise.allSettled([
        p.api.data('/update/status?nodes=' + encodeURIComponent(name), { signal: reader.scope.signal, timeoutMs: 45000 }),
        p.api.data('/update/history?nodes=' + encodeURIComponent(name), { signal: reader.scope.signal, timeoutMs: 45000 }),
      ]);
      if (!reader.current() || version !== selectionVersion || automatic && !watch.canUpdate()) return false;
      note.replaceChildren(); body.replaceChildren();
      const row = observed.status === 'fulfilled' ? observed.value.nodes?.find(row => row.node === name) : null;
      if (row?.status) body.append(statusDetails(row.status));
      else note.append(row?.error ? message(row.error, 'warning') : observed.status === 'rejected' ? problem(observed.reason) : message('This node did not return update status.', 'warning'));
      const past = history.status === 'fulfilled' ? history.value.nodes?.find(row => row.node === name) : null;
      if (past?.history) body.append(el('details', { class: 'access-section' }, el('summary', {}, 'Installation history'),
        ...(past.history.entries || []).map(entry => text(`${date(entry.timestamp)} · ${versionText(entry.from_version)} → ${versionText(entry.to_version)} · ${entry.success ? 'Succeeded' : entry.error || 'Failed'}`))));
      else note.append(past?.error ? message(past.error, 'warning') : history.status === 'rejected' ? problem(history.reason) : message('Installation history is unavailable.', 'warning'));
      return !!row?.status && !!past?.history;
    }, { eligible: () => !reader.editing && !body.contains(document.activeElement), failed: error => note.replaceChildren(problem(error)) });
    await watch.refresh();
  }
  async function resultView(owner, result, notice, error) {
    const reader = owner.panel.view('Update result');
    reader.root.append(el('h2', {}, 'Update request'), error ? problem(error) : message(notice),
      result ? facts([['Target version', result.version], ['State', result.state], ['Job ID', result.job_id], ['Delegated to', result.delegated_to]]) : '',
      text('The coordinator may restart or delegate the work. An interrupted response does not prove failure. This request will not be repeated automatically.', 'muted'),
      actions(leaveButton('Inspect update state', listURL, p.api), result?.job_id ? leaveButton('View update job', () => p.go('workloads', workloadKey('job', p.state.node_name, result.job_id), 'jobs'), p.api) : null));
  }
  function updateForm(owner) {
    const editor = owner.panel.view('Install software version'), form = el('form', { class: 'entity-editor' });
    const controls = el('fieldset', { 'data-protected-actions': true });
    const target = input('version', '', { required: true, placeholder: 'For example, 0.2.3' });
    const choices = rows.map(row => ({ name: row.node, control: input('target_' + row.node, '', { type: 'checkbox' }) }));
    const feedback = el('div', { 'aria-live': 'polite' }), validation = formFeedback(form, feedback, key => key === 'version' ? target : key === 'nodes' ? choices[0]?.control : undefined);
    const draft = editor.trackDraft(form); let busy = false;
    controls.append(el('h2', {}, 'Choose release and targets'), field('Release version', target), el('h3', {}, 'Compute nodes'),
      ...choices.map(item => field(item.name, item.control)), message('Selected nodes are updated serially. Running workloads may be interrupted. The API checks release compatibility before accepting the rollout.', 'warning'));
    form.append(feedback, controls, editorActions(el('button', { type: 'submit', class: 'primary' }, 'Install selected version'), listURL, p.api)); editor.root.append(form);
    form.addEventListener('submit', async event => {
      event.preventDefault(); if (busy || !form.reportValidity() || !p.api.canChange()) return;
      const nodes = choices.filter(item => item.control.checked).map(item => item.name);
      if (!nodes.length) { validation.error(new Error('Select at least one compute node.')); return; }
      busy = true; controls.disabled = true;
      try {
        const result = await p.api.data('/update/apply', { method: 'POST', body: { version: target.value.trim(), nodes }, idempotencyKey: crypto.randomUUID(), signal: editor.scope.signal, timeoutMs: 120000 });
        if (editor.current()) { draft.clean(); await resultView(owner, result, 'Update request accepted. Inspect the rollout and node versions to confirm the result.'); }
      } catch (error) {
        if (!editor.current()) return;
        if (error.uncertain || error.mutation_applied) { draft.clean(); await resultView(owner, null, '', error); }
        else { validation.error(error); controls.disabled = false; busy = false; }
      }
    });
  }
  async function scheduleForm(owner, name) {
    const editor = owner.panel.view('Update schedule'), path = '/update/settings?node=' + encodeURIComponent(name);
    editor.root.append(message('Reading schedule…'));
    try {
      const initial = await p.api.data(path, { signal: editor.scope.signal }); if (!editor.current()) return;
      const control = input('enabled', '', { type: 'checkbox', checked: initial.enabled });
      const form = el('form', { class: 'entity-editor' }), feedback = el('div'), validation = formFeedback(form, feedback);
      const controls = el('fieldset', { 'data-protected-actions': true }, el('h2', {}, name), field('Enable scheduled software updates', control));
      const draft = editor.trackDraft(form); let busy = false;
      form.append(feedback, controls, editorActions(el('button', { type: 'submit', class: 'primary' }, 'Save changes'), () => nodeDetails(owner, name, selectionVersion), p.api)); editor.root.replaceChildren(form);
      form.addEventListener('submit', async event => {
        event.preventDefault(); if (busy || !p.api.canChange()) return; busy = true; controls.disabled = true;
        try {
          const current = await p.api.data(path, { signal: editor.scope.signal });
          if (!editor.current()) return;
          if (current.enabled !== initial.enabled) throw new Error('The schedule changed elsewhere. Close this editor and read the current settings before saving.');
          await p.api.data(path, { method: 'PATCH', body: { enabled: control.checked }, signal: editor.scope.signal });
          if (!editor.current()) return; draft.clean();
          await readAfterWrite(owner.panel.view('Node software'), () => nodeDetails(owner, name, selectionVersion, 'Update schedule saved.'));
        } catch (error) { if (editor.current()) { validation.error(error); controls.disabled = !!error.uncertain; busy = !!error.uncertain; } }
      });
    } catch (error) { if (editor.current()) editor.root.replaceChildren(problem(error)); }
  }
  p.handleSelection?.((value, mode) => choose(value?.startsWith('updates/') ? decodeURIComponent(value.slice(8)) : undefined, mode), value => value === 'updates' || value?.startsWith('updates/'));
  await live.refresh(); if (p.current()) await choose(selected, mode);
}
