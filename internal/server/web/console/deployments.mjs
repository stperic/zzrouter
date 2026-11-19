import { el, text, button, leaveButton, actions, heading, field, input, facts, message, problem, badge, selectableRow, markSelection, date } from './dom.mjs';
import { details } from './details.mjs';
import { liveReads } from './live-reads.mjs';
import { bytes, deploymentTerminal } from './model-data.mjs';
import { workloadKey } from './workload-data.mjs';

const path = id => `/deployments/${encodeURIComponent(id)}`;

export function deploymentSummary(record) {
  return el('div', {}, el('h2', {}, record.model), actions(badge(record.status), badge(record.registry || 'Registry not reported')),
    facts([['Deployment', record.id], ['Created', date(record.created_at, 'Not reported')], ['Variant', record.file || 'Registry selection'], ['Progress', `${record.nodes_complete ?? 0}/${record.nodes_total ?? record.nodes?.length ?? 0} nodes complete`]]),
    record.message ? text(record.message, 'muted') : null);
}

export async function inspectDeployment(p, id, initial) {
  const feedback = el('div', { 'aria-live': 'polite' }), content = el('div');
  let record = initial, changing = false, blocked = false;
  const refresh = leaveButton('Refresh', () => live.refresh(), p.api);
  const pause = button('Pause updates', () => live.toggle());
  p.root.replaceChildren(actions(refresh, pause), feedback, content);
  const render = () => {
    content.replaceChildren(deploymentSummary(record));
    if (record.restart_job_id) content.append(message('Restarting affected runs is tracked separately from the download.'), leaveButton('Inspect restart job', () => p.go('workloads', workloadKey('job', p.state.node_name, record.restart_job_id), 'jobs'), p.api));
    if (!deploymentTerminal(record.status)) content.append(mutationButton('Cancel download', () => cancel()));
    for (const item of record.nodes || []) {
      const progress = Number.isFinite(item.progress) ? `${Math.round(item.progress)}%` : 'Progress not reported';
      const row = el('section', { class: 'card' }, el('h3', {}, item.node), actions(badge(item.status)),
        facts([['Downloaded', `${bytes(item.bytes_downloaded)} / ${bytes(item.bytes_total)}`], ['Progress', progress], ['Speed', Number.isFinite(item.speed) && item.speed > 0 ? `${bytes(item.speed)}/s` : 'Not reported']]),
        item.error ? message(item.error, 'warning') : null,
        item.job_id ? leaveButton('View job', () => p.go('workloads', workloadKey('job', item.node, item.job_id), 'jobs'), p.api) : null,
        !deploymentTerminal(item.status) ? mutationButton('Cancel on this node', () => cancel(item.node)) : null);
      content.append(row);
    }
    if (deploymentTerminal(record.status)) content.append(leaveButton('Inspect available models', () => p.go('models'), p.api), text('Download status is reported by the coordinator. Available models are read separately before starting a run.', 'muted'));
  };
  function mutationButton(label, action) {
    const control = button(label, action, 'danger');
    control.setAttribute('data-protected-actions', ''); control.disabled = blocked || changing;
    return control;
  }
  async function cancel(node) {
    if (changing || blocked || !p.current() || !p.api.canChange() || !confirm(`Cancel ${record.model}${node ? ` on ${node}` : ' on all selected nodes'}? Completed model files are not removed.`)) return;
    changing = true; blocked = true; render();
    await live.settle();
    if (!p.current()) return;
    try {
      await p.api.request('/zzrouter/v1' + path(id) + (node ? `/nodes/${encodeURIComponent(node)}` : ''), { method: 'DELETE', signal: p.scope.signal });
      if (p.current()) feedback.replaceChildren(message('Cancellation acknowledged. Inspecting current progress…'));
    } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
    finally {
      changing = false;
      if (p.current()) { await live.refresh(); feedback.append(text('No cancellation is replayed. Refresh to inspect the current state.', 'muted')); }
    }
  }
  const live = liveReads(p, async automatic => {
    try {
      const next = await p.api.data(path(id), { signal: p.scope.signal });
      if (!p.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      record = next; render();
      return true;
    } catch (error) {
      if (p.current()) feedback.replaceChildren(error.status === 404 ? message('This deployment is no longer retained. Its disappearance does not establish success.', 'warning') : problem(error));
      return false;
    }
  }, { eligible: () => !changing && !deploymentTerminal(record?.status) && !content.contains(document.activeElement), changed: () => { pause.textContent = live.paused ? 'Resume updates' : 'Pause updates'; } });
  if (record) render();
  await live.refresh();
}

export async function deployments(p, selected) {
  const listing = el('div', { class: 'entity-list access-list' }), workspace = el('div', { class: 'entity-workspace' }, listing);
  const search = input('download_search', '', { type: 'search', placeholder: 'Search models, nodes or status' });
  const results = el('div'), feedback = el('div');
  let records = [], drawer;
  const choose = async id => {
    drawer?.dispose(!id); drawer = undefined; selected = id; markSelection(results, selected);
    if (!id) return;
    drawer = details({ ...p, root: workspace }, 'Download details', () => p.go('workloads', undefined, 'downloads'));
    await inspectDeployment(drawer, id, records.find(row => row.id === id));
  };
  const render = () => {
    const query = search.value.trim().toLowerCase();
    const rows = records.filter(row => `${row.model} ${row.registry} ${row.status} ${(row.nodes || []).map(node => node.node).join(' ')}`.toLowerCase().includes(query));
    results.replaceChildren(rows.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Model', 'Status', 'Nodes', 'Updated'].map(value => el('th', { scope: 'col' }, value)))),
      el('tbody', {}, rows.map(row => selectableRow(row.id, selected, () => p.go('workloads', row.id, 'downloads'),
        el('td', {}, leaveButton(row.model, () => p.go('workloads', row.id, 'downloads'), p.api, 'link entity-title'), text(row.registry || '', 'muted')),
        el('td', {}, row.status), el('td', {}, `${row.nodes_complete || 0}/${row.nodes_total || row.nodes?.length || 0}`), el('td', {}, date(row.updated_at, 'Not reported'))))))) : null,
      rows.length ? text(`${rows.length} ${rows.length === 1 ? 'download' : 'downloads'}`, 'muted') : message(query ? 'No downloads match this search.' : 'No downloads yet. Find a model in Discover to add one.'));
  };
  const read = async () => {
    try { const next = await p.api.list('/deployments', { signal: p.scope.signal }); if (p.current()) { records = next; render(); feedback.replaceChildren(); } }
    catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
  };
  p.root.append(heading('Workloads', 'Running models, downloads and background jobs.', leaveButton('Refresh', read, p.api)),
    el('nav', { class: 'actions tabs', 'aria-label': 'Workload views' }, [['runs', 'Runs'], ['jobs', 'Jobs'], ['downloads', 'Downloads']].map(([id, label]) => { const control = leaveButton(label, () => p.go('workloads', undefined, id), p.api); control.setAttribute('aria-current', String(id === 'downloads')); return control; })), workspace);
  listing.append(field('Search downloads', search), actions(leaveButton('Discover models', () => p.go('models', undefined, 'discover'), p.api)), feedback, results);
  search.addEventListener('input', render);
  await read(); if (!p.current()) return;
  p.handleSelection?.(choose, (_item, mode) => mode === 'downloads');
  if (selected) await choose(selected);
}
