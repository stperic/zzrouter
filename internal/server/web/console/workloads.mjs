import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, heading, card, actions, select, field, input, badge, message, problem, selectableRow, markSelection } from './dom.mjs';
import { details } from './details.mjs';
import { liveReads } from './live-reads.mjs';
import { activityText } from './observations.mjs';
import { readWorkloadPages, workloadKey, workloadSelection, newerJob, jobSubject, jobProgress } from './workload-data.mjs';
import { inspectWorkload } from './workload-details.mjs';
import { startRun } from './run-form.mjs';

export async function workloads(p, selection, mode = 'runs') {
  const initial = workloadSelection(selection);
  let tab = mode === 'jobs' || initial.kind === 'job' ? 'jobs' : 'runs', selected = selection, drawer, loading = false, selectionVersion = 0;
  const reads = new Lifetime(), snapshots = new Map();
  let nodes = [], scope = reads.next(), offsets = new Map(), more = new Map(), filterNode = initial.node || '';
  const node = select('workload_node', [['', 'All nodes']], initial.node || '');
  node.disabled = true;
  const search = input('workload_search', '', { type: 'search', placeholder: 'Search models, providers or jobs', 'aria-label': 'Search workloads' });
  const feedback = el('div', { 'aria-live': 'polite' }), results = el('div'), tabs = el('div', { class: 'tabs', 'aria-label': 'Workload type' });
  const list = card('Current workloads', el('div', { class: 'workload-filters' }, field('Compute node', node), field('Search', search)), feedback, results);
  list.classList.add('entity-list');
  const workspace = el('div', { class: 'entity-workspace' }, list);
  const pause = button('Pause updates', () => live.toggle());
  const idle = () => !drawer && !list.contains(document.activeElement) && p.api.canChange();
  const live = liveReads(p, (automatic, older = false) => read(older, automatic), { eligible: idle, changed: () => { pause.textContent = live.paused ? 'Resume updates' : 'Pause updates'; pause.setAttribute('aria-pressed', String(live.paused)); }, failed: error => feedback.replaceChildren(problem(error)) });
  p.root.append(heading('Workloads', 'Running models, node services and background jobs.', leaveButton('Refresh', () => live.refresh(), p.api), pause,
    leaveButton('+ Start run', () => p.go('workloads', node.value ? workloadKey('node', node.value, '') : undefined, 'create'), p.api, 'primary')), tabs, workspace);
  p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });

  function tabControls() {
    tabs.replaceChildren(...['runs', 'jobs'].map(value => {
      const control = leaveButton(value === 'runs' ? 'Runs' : 'Jobs', () => p.go('workloads', node.value ? workloadKey('node', node.value, '') : undefined, value), p.api);
      control.setAttribute('aria-current', String(value === tab)); return control;
    }));
  }
  function render() {
    const query = search.value.trim().toLowerCase();
    const rows = [...snapshots.values()].flat().filter(row => [row.model, row.provider, row.id, row.job_id, row.kind, jobSubject(row)].some(value => String(value || '').toLowerCase().includes(query)));
    const body = el('tbody');
    for (const row of rows) {
      const id = tab === 'runs' ? row.id : row.job_id;
      const key = workloadKey(tab === 'runs' ? 'run' : 'job', row.node, id);
      const open = () => p.go('workloads', key, tab);
      body.append(selectableRow(key, selected, open,
        el('td', {}, leaveButton(tab === 'runs' ? row.model || `${row.provider} service` : jobSubject(row), open, p.api, 'link'), text(tab === 'runs' ? row.provider : row.kind, 'muted')),
        el('td', {}, row.node), el('td', {}, badge(tab === 'runs' ? row.record_only ? 'Stopped record' : row.status : row.phase), row.lastReported ? text('Last reported; refresh failed', 'muted') : null),
        el('td', {}, tab === 'runs' ? activityText(p, row.activity) : jobProgress(row))));
    }
    const older = button('Load older jobs', () => live.refresh(true, true)); older.disabled = loading;
    results.replaceChildren(...[rows.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Workload', 'Node', 'Status', tab === 'runs' ? 'Activity' : 'Progress'].map(value => el('th', { scope: 'col' }, value)))), body)) : message(query ? 'No matching workloads.' : 'No workloads in the current successful observations.'),
      tab === 'jobs' ? text('Job order changes as events arrive. Older pages may overlap or omit jobs that moved between pages; Refresh reloads the latest page.', 'muted') : null,
      tab === 'jobs' && [...more.values()].some(Boolean) ? older : null].filter(Boolean));
  }
  async function read(older = false, automatic = false) {
    if (loading) return true;
    loading = true;
    const own = scope;
    const reader = { ...p, scope: { signal: AbortSignal.any([p.scope.signal, own.signal]) }, current: () => p.current() && own.current() };
    try {
      if (!nodes.length || !automatic && !older) {
        nodes = await p.api.list('/nodes', { signal: reader.scope.signal });
        if (!reader.current()) return false;
        node.replaceChildren(el('option', { value: '' }, 'All nodes'), ...nodes.map(value => el('option', { value: value.name }, value.name)));
        if (filterNode && !nodes.some(value => value.name === filterNode)) node.append(el('option', { value: filterNode }, `${filterNode} (not in current inventory)`));
        node.value = filterNode;
        node.disabled = false;
      }
      const names = node.value ? [node.value] : nodes.map(value => value.name);
      const requested = older ? names.filter(name => more.get(name)) : names;
      const nextOffsets = new Map(requested.map(name => [name, older ? (offsets.get(name) || 0) + 25 : 0]));
      const pages = await readWorkloadPages(reader, requested, tab, nextOffsets);
      if (!reader.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      if (!older) for (const name of snapshots.keys()) if (!names.includes(name)) { snapshots.delete(name); more.delete(name); offsets.delete(name); }
      feedback.replaceChildren();
      for (const page of pages) {
        if (page.error) {
          feedback.append(message(`Could not read ${tab} on ${page.node}.`, 'warning'), problem(page.error));
          if (snapshots.has(page.node)) snapshots.set(page.node, snapshots.get(page.node).map(row => ({ ...row, lastReported: true })));
          continue;
        }
        const values = new Map((older ? snapshots.get(page.node) || [] : []).map(row => [row.job_id, row]));
        if (tab === 'jobs') for (const row of page.rows) values.set(row.job_id, newerJob(values.get(row.job_id), row));
        snapshots.set(page.node, tab === 'jobs' ? [...values.values()] : page.rows);
        more.set(page.node, page.more); offsets.set(page.node, page.offset);
      }
      loading = false;
      render(); return !pages.some(page => page.error);
    } finally { loading = false; }
  }
  async function choose(next, nextMode) {
    const version = ++selectionVersion;
    const current = () => p.current() && version === selectionVersion;
    const identity = workloadSelection(next);
    const desired = nextMode === 'jobs' || identity.kind === 'job' ? 'jobs' : 'runs';
    drawer?.dispose(!next); drawer = undefined; selected = next;
    const desiredNode = identity.kind === 'node' ? identity.node : !next ? '' : filterNode;
    if (desired !== tab || desiredNode !== filterNode) {
      tab = desired; filterNode = desiredNode; node.value = filterNode;
      const own = scope = reads.next();
      await live.settle();
      if (!current() || !own.current()) return;
      snapshots.clear(); offsets = new Map(); more = new Map();
      tabControls(); await live.refresh();
      if (!current()) return;
    }
    markSelection(list, next);
    if (nextMode !== 'create' && !['run', 'job'].includes(identity.kind)) return;
    const owner = details({ ...p, root: workspace }, nextMode === 'create' ? 'Start run' : identity.kind === 'job' ? 'Job details' : 'Run details', () => p.go('workloads', node.value ? workloadKey('node', node.value, '') : undefined, tab));
    drawer = owner;
    if (nextMode === 'create') await startRun(owner, nodes, node.value);
    else if (identity.node && identity.id) await inspectWorkload(owner, identity.kind === 'run' ? 'runs' : 'jobs', identity.node, identity.id);
    else owner.root.append(message('This workload identity is incomplete.', 'warning'));
  }
  node.addEventListener('change', () => {
    const value = node.value; node.value = filterNode;
    if (p.canLeave?.() ?? p.api.canLeave()) void p.go('workloads', value ? workloadKey('node', value, '') : undefined, tab);
  });
  search.addEventListener('input', render);
  tabControls();
  feedback.append(message('Reading node workloads…'));
  await live.refresh();
  if (p.current()) { p.handleSelection?.(choose); await choose(selected, mode); }
}
