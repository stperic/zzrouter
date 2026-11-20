import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, heading, actions, select, field, input, badge, message, problem, selectableRow, markSelection } from './dom.mjs';
import { details } from './details.mjs';
import { liveReads } from './live-reads.mjs';
import { activityText } from './observations.mjs';
import { readWorkloadPages, workloadKey, workloadSelection, newerJob, jobSubject, jobProgress, stoppedRun, workloadPageSize } from './workload-data.mjs';
import { inspectWorkload } from './workload-details.mjs';
import { startRun } from './run-form.mjs';
import { calibrationForm } from './calibrations.mjs';

const tabFor = (mode, identity) => ['calibrations', 'create-calibration'].includes(mode) ? 'calibrations' : mode === 'jobs' || identity.kind === 'job' ? 'jobs' : 'runs';

export async function workloads(p, selection, mode = 'runs') {
  const initial = workloadSelection(selection);
  let tab = tabFor(mode, initial), selected = selection, drawer, loading = false, selectionVersion = 0;
  const reads = new Lifetime(), snapshots = new Map();
  let nodes = [], scope = reads.next(), offsets = new Map(), more = new Map(), filterNode = initial.node || '';
  const node = select('workload_node', [['', 'All nodes']], initial.node || '');
  node.disabled = true;
  const search = input('workload_search', '', { type: 'search', placeholder: 'Search models, providers or jobs', 'aria-label': 'Search workloads' });
  const visibility = select('workload_status', [['active', 'Active workloads'], ['stopped', 'Stopped records'], ['all', 'All workloads']], 'active');
  const statusFilter = field('Show', visibility);
  const feedback = el('div', { 'aria-live': 'polite' }), results = el('div'), tabs = el('div', { class: 'tabs', 'aria-label': 'Workload type' });
  const list = el('section', { class: 'card entity-list' }, el('div', { class: 'workload-filters' }, field('Compute node', node), statusFilter, field('Search', search)), feedback, results);
  const workspace = el('div', { class: 'entity-workspace' }, list);
  const pause = button('Pause updates', () => live.toggle());
  const idle = () => !drawer && !list.contains(document.activeElement) && p.api.canChange();
  const live = liveReads(p, (automatic, older = false) => read(older, automatic), { eligible: idle, changed: () => { pause.textContent = live.paused ? 'Resume updates' : 'Pause updates'; pause.setAttribute('aria-pressed', String(live.paused)); }, failed: error => feedback.replaceChildren(problem(error)) });
  const nodeSelection = () => node.value ? workloadKey('node', node.value, '') : undefined;
  const create = leaveButton('', () => p.go('workloads', nodeSelection(), tab === 'calibrations' ? 'create-calibration' : 'create'), p.api, 'primary');
  p.root.append(heading('Workloads', 'Running models, node services and background jobs.', leaveButton('Refresh', () => live.refresh(), p.api), pause, create), tabs, workspace);
  p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });

  function tabControls() {
    statusFilter.hidden = tab !== 'runs';
    create.textContent = tab === 'calibrations' ? 'New calibration' : '+ Start run';
    tabs.replaceChildren(...[['runs', 'Runs'], ['jobs', 'Jobs'], ['calibrations', 'Calibrations'], ['downloads', 'Downloads']].map(([value, label]) => {
      const control = leaveButton(label, () => p.go('workloads', value === 'downloads' ? undefined : nodeSelection(), value), p.api);
      control.setAttribute('aria-current', String(value === tab)); return control;
    }));
  }
  function render() {
    const query = search.value.trim().toLowerCase();
    const rows = [...snapshots.values()].flat().filter(row => [row.model, row.provider, row.id, row.job_id, row.kind, jobSubject(row)].some(value => String(value || '').toLowerCase().includes(query)))
      .filter(row => tab !== 'runs' || visibility.value === 'all' || workloadKey('run', row.node, row.id) === selected || stoppedRun(row) === (visibility.value === 'stopped'));
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
    results.replaceChildren(...[rows.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Workload', 'Node', 'Status', tab === 'runs' ? 'Activity' : 'Progress'].map(value => el('th', { scope: 'col' }, value)))), body)) : el('div', {}, message(query ? 'No matching workloads.' : 'No workloads match this view in the successful observations.'),
      tab === 'runs' && visibility.value === 'active' && !query ? leaveButton('Start a run', () => p.go('workloads', node.value ? workloadKey('node', node.value, '') : undefined, 'create'), p.api) : null),
      tab !== 'runs' ? text('Job order changes as events arrive. Older pages may overlap or omit jobs that moved between pages; Refresh reloads the latest page.', 'muted') : null,
      tab !== 'runs' && [...more.values()].some(Boolean) ? older : null].filter(Boolean));
  }
  async function read(older = false, automatic = false) {
    if (loading) return true;
    loading = true;
    const own = scope;
    const reader = { ...p, scope: { signal: AbortSignal.any([p.scope.signal, own.signal]) }, current: () => p.current() && own.current() };
    try {
      if (!nodes.length || !older) {
        const latest = await p.api.list('/nodes', { signal: reader.scope.signal });
        if (!reader.current()) return false;
        if (automatic && !live.canUpdate()) return true;
        nodes = latest;
        node.replaceChildren(el('option', { value: '' }, 'All nodes'), ...nodes.map(value => el('option', { value: value.name }, `${value.name}${value.health_status === 'down' ? ' (offline)' : ''}`)));
        if (filterNode && !nodes.some(value => value.name === filterNode)) node.append(el('option', { value: filterNode }, `${filterNode} (not in current inventory)`));
        node.value = filterNode;
        node.disabled = false;
      }
      const missing = Boolean(node.value && !nodes.some(value => value.name === node.value));
      const names = missing ? [] : node.value ? [node.value] : nodes.map(value => value.name);
      const offline = nodes.filter(value => value.health_status === 'down' && names.includes(value.name)).map(value => value.name);
      const requested = names.filter(name => !offline.includes(name) && (!older || more.get(name)));
      const nextOffsets = new Map(requested.map(name => [name, older ? (offsets.get(name) || 0) + workloadPageSize : 0]));
      const pages = await readWorkloadPages(reader, requested, tab === 'runs' ? 'runs' : 'jobs', nextOffsets, tab === 'calibrations' ? 'calibration' : undefined);
      if (!reader.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      if (!older) for (const name of snapshots.keys()) if (!names.includes(name) || offline.includes(name)) { snapshots.delete(name); more.delete(name); offsets.delete(name); }
      feedback.replaceChildren();
      if (missing) feedback.append(message('This node is no longer in the current inventory.', 'warning'));
      if (offline.length) feedback.append(text(node.value ? 'This node is offline. Workloads will appear when it reconnects.' : `${offline.length} offline ${offline.length === 1 ? 'node' : 'nodes'} skipped.`, 'muted'));
      for (const page of pages) {
        if (page.error) {
          feedback.append(el('div', { class: 'notice warning' }, text(`Could not refresh ${tab} on ${page.node}.`),
            el('details', {}, el('summary', {}, 'Technical details'), problem(page.error))));
          if (snapshots.has(page.node)) snapshots.set(page.node, snapshots.get(page.node).map(row => ({ ...row, lastReported: true })));
          continue;
        }
        const values = new Map((older ? snapshots.get(page.node) || [] : []).map(row => [row.job_id, row]));
        if (tab !== 'runs') for (const row of page.rows) values.set(row.job_id, newerJob(values.get(row.job_id), row));
        snapshots.set(page.node, tab === 'runs' ? page.rows : [...values.values()]);
        more.set(page.node, page.more); offsets.set(page.node, page.offset);
      }
      if (pages.some(page => page.error)) feedback.append(leaveButton('Retry reads', () => live.refresh(), p.api));
      loading = false;
      render(); return !pages.some(page => page.error);
    } finally { loading = false; }
  }
  async function choose(next, nextMode) {
    const version = ++selectionVersion;
    const current = () => p.current() && version === selectionVersion;
    const identity = workloadSelection(next);
    const creating = ['create', 'create-calibration'].includes(nextMode);
    const previous = selected, closing = !!drawer && !creating && !['run', 'job'].includes(identity.kind);
    const desired = tabFor(nextMode, identity);
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
    render(); markSelection(list, next);
    if (closing) ([...list.querySelectorAll('[data-selection]')].find(row => row.getAttribute('data-selection') === previous)?.querySelector?.('button') || search).focus({ preventScroll: true });
    if (!creating && !['run', 'job'].includes(identity.kind)) return;
    const title = nextMode === 'create-calibration' ? 'New calibration' : creating ? 'Start run' : identity.kind === 'job' ? 'Job details' : 'Run details';
    const owner = details({ ...p, root: workspace }, title, () => p.go('workloads', nodeSelection(), tab));
    drawer = owner;
    if (nextMode === 'create') await startRun(owner, nodes, node.value);
    else if (nextMode === 'create-calibration') await calibrationForm(owner, { node: node.value, nodes });
    else if (identity.node && identity.id) await inspectWorkload(owner, identity.kind === 'run' ? 'runs' : 'jobs', identity.node, identity.id);
    else owner.root.append(message('This workload identity is incomplete.', 'warning'));
  }
  node.addEventListener('change', () => {
    const value = node.value; node.value = filterNode;
    if (p.canLeave?.() ?? p.api.canLeave()) void p.go('workloads', value ? workloadKey('node', value, '') : undefined, tab);
  });
  search.addEventListener('input', render);
  visibility.addEventListener('change', render);
  tabControls();
  feedback.append(message('Reading node workloads…'));
  await live.refresh();
  if (p.current()) { p.handleSelection?.(choose, (_, nextMode) => nextMode !== 'downloads'); await choose(selected, mode); }
}
