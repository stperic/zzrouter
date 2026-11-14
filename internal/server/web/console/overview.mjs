import { el, text, button, leaveButton, heading, badge, message, problem, date, facts, actions } from './dom.mjs';
import { readInventory } from './inventory.mjs';
import { nodeSummary, clientKey, nodeKey } from './network-data.mjs';
import { readClients, showClient, connectionGuide } from './clients.mjs';
import { showNode, showLocalProvider, addNode } from './nodes.mjs';
import { details } from './details.mjs';
import { topologyData } from './topology-data.mjs';
import { topology } from './topology.mjs';
import { providerName } from './provider-icons.mjs';
import { showProvider } from './providers.mjs';
import { activityText, memorySummary } from './observations.mjs';
import { liveReads } from './live-reads.mjs';

export async function overview(p, selection) {
  let snapshot, model, graph, drawer;
  let selected = selection;
  let listMode = false;
  let graphFailed = false;
  const feedback = el('div', { 'aria-live': 'polite' });
  const freshness = el('span', { class: 'network-caption', role: 'status' });
  const counts = el('div', { class: 'network-counts' });
  const graphStatus = el('div', { class: 'topology-status', role: 'status' }, 'Arranging your network…');
  const canvas = el('div', { class: 'topology-canvas', tabindex: '0', role: 'img', 'aria-label': 'Network topology. Use List view to select clients, nodes and providers with the keyboard.' });
  const list = el('div', { class: 'topology-list', hidden: true });
  const zoomOut = button('−', () => graph?.zoom(0.8));
  zoomOut.setAttribute('aria-label', 'Zoom out');
  const zoomIn = button('+', () => graph?.zoom(1.25));
  zoomIn.setAttribute('aria-label', 'Zoom in');
  const fit = button('Fit', () => graph?.fit());
  fit.setAttribute('aria-label', 'Fit network to view');
  const view = button('List view', () => setListMode(!listMode));
  const stage = el('section', { class: 'topology-stage entity-list', 'aria-label': 'Network map' },
    el('div', { class: 'topology-toolbar' }, text('Network', 'topology-title'), actions(zoomOut, zoomIn, fit, view)),
    graphStatus, canvas, list,
    el('div', { class: 'topology-legend' }, el('span', {}, '● Healthy / active'), el('span', { class: 'legend-down' }, '● Down'), el('span', { class: 'legend-degraded' }, '● Degraded'), el('span', {}, '● Unknown / last observed'), text('Click an item for details. Drag to pan.')));
  const workspace = el('div', { class: 'network-workspace entity-workspace' }, stage);
  const body = el('div', { class: 'network-overview' }, counts, workspace);
  const refreshButton = leaveButton('Refresh', async () => {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    const panel = drawer, revision = panel?.panel.revision;
    if (await read() && drawer === panel && drawer?.panel.revision === revision && drawer?.editing && p.api.canLeave()) select(selected, true);
  }, p.api);
  const pause = button('Pause updates', () => live.toggle());
  p.root.append(heading('Overview', 'Your clients, coordinator and compute nodes.',
    refreshButton, pause, leaveButton('+ Add node', () => p.go('overview', 'add'), p.api, 'primary')), feedback, body,
    el('div', { class: 'network-footer' }, freshness, actions(leaveButton('+ Connect client', () => p.go('overview', 'connect'), p.api, 'link'), leaveButton('+ Cloud provider', () => p.go('providers', 'cloud'), p.api, 'link'))),
    text('Memory shows used / total GiB. * marks last reported readings. A pulse means observed work. Node health is the coordinator’s latest observation; configured providers and authorized clients do not establish a live connection.', 'network-caption'));
  const live = liveReads(p, readSnapshot, { interval: 15000, eligible: () => idle(), changed: () => {
    pause.textContent = live.paused ? 'Resume updates' : 'Pause updates';
    pause.setAttribute('aria-pressed', String(live.paused));
    graph?.pause(live.paused);
    freshness.textContent = live.failed ? 'Automatic updates stopped. Refresh to retry.' : live.paused ? 'Updates and motion paused.' : 'Refreshes every 15 seconds while idle.';
  } });
  const read = () => live.refresh();

  function setListMode(value) {
    listMode = value;
    list.hidden = !value;
    canvas.hidden = value;
    view.textContent = value ? 'Map view' : 'List view';
    view.setAttribute('aria-pressed', String(value));
    for (const control of [zoomOut, zoomIn, fit]) control.disabled = value || !graph;
  }

  function select(next, reload = false) {
    if (!p.current() || !p.api.canLeave()) return;
    if (next === selected && drawer && !reload) return;
    const before = selected;
    drawer?.dispose();
    drawer = undefined;
    selected = next;
    graph?.select(next);
    for (const item of list.querySelectorAll('[data-selection]')) item.setAttribute('aria-pressed', String(item.getAttribute('data-selection') === next));
    if (!next) {
      if (before) (listMode ? [...list.querySelectorAll('[data-selection]')].find(item => item.getAttribute('data-selection') === before) : canvas)?.focus({ preventScroll: true });
      return;
    }
    const title = next === 'add' ? 'Add node' : next === 'connect' ? 'Connect client' : next.startsWith('client/') ? 'Client details' : next.startsWith('cloud/') ? 'Cloud provider' : next.startsWith('local/') ? 'Local provider' : 'Node details';
    const owner = details({ ...p, root: workspace, reloadNode: async name => {
      const revision = owner.panel.revision;
      if (await read() && owner.panel.current() && owner.panel.revision === revision && p.api.canLeave()) { const selection = next.startsWith('local/') ? next : nodeKey(name); p.selectSection?.(selection); select(selection, true); }
    } }, title, () => p.go('overview'));
    drawer = owner;
    if (next === 'add') addNode(owner);
    else if (next === 'connect') owner.root.append(connectionGuide(owner));
    else if (next.startsWith('node/')) showNode(owner, next.slice(5), snapshot.inventory);
    else if (next.startsWith('local/')) {
      const entry = model.elements.find(({ data }) => data.kind === 'provider' && data.selection === next)?.data;
      if (entry) showLocalProvider(owner, entry.nodeName, entry.providerKey, snapshot.inventory);
      else owner.root.append(message('This provider is no longer in the current node inventory.', 'warning'));
    } else if (next.startsWith('client/')) showClient(owner, model.clients.find(row => clientKey(row) === next.slice(7)));
    else if (next.startsWith('cloud/')) {
      const entry = snapshot.catalog.value?.find(row => row.key === next.slice(6));
      if (entry) void showProvider(owner, entry, snapshot.inventory, () => read());
      else owner.root.append(message('This cloud provider is reported by a worker. Configure its connection on that node.', 'warning'));
    } else owner.root.append(message('This item is not in the current network.', 'warning'));
    p.syncActivity();
  }

  function updateModel() {
    model = topologyData(snapshot, p.state.node_name);
    const summary = nodeSummary(snapshot.inventory);
    counts.replaceChildren(...(summary ? [el('span', {}, el('strong', {}, `${summary.healthy} / ${summary.total}`), ' compute nodes healthy'),
      summary.down ? badge(`${summary.down} down`, 'bad') : null, summary.degraded ? badge(`${summary.degraded} degraded`, 'warn') : null,
      summary.unknown ? badge(`${summary.unknown} unknown`) : null] : [message('Node counts unavailable.', 'warning')]).filter(Boolean));
    const active = model.clients.filter(row => row.active_requests > 0).length;
    counts.append(text(snapshot.clientResults[0].status === 'rejected' ? 'Client activity unavailable' : `${active} active ${active === 1 ? 'identity' : 'identities'} at refresh · ${model.clients.length} observed`, 'network-caption'));
    list.replaceChildren(...['client', 'coordinator', 'compute', 'cloud', 'provider'].map(kind => {
      const entries = model.elements.filter(element => element.data.kind === kind);
      if (!entries.length) return null;
      const title = { client: 'Clients', coordinator: 'Coordinator', compute: 'Compute nodes', cloud: 'Cloud providers', provider: 'Local providers' }[kind];
      return el('section', {}, el('h2', {}, title), entries.map(({ data }) => {
        const item = leaveButton('', () => p.go('overview', data.selection), p.api, 'topology-list-item');
        item.setAttribute('data-selection', data.selection);
        item.setAttribute('aria-pressed', String(data.selection === selected));
        if (['coordinator', 'compute'].includes(kind)) item.dataset.node = data.name;
        item.append(data.providerKey ? providerName(data.providerKey, data.nodeName ? `${data.name} · ${data.nodeName}` : data.name) : el('span', {}, data.name), badge(data.status, data.tone));
        if (kind === 'compute') { const node = model.nodes.find(node => node.name === data.name); item.append(el('span', { class: 'memory-summary' }, memorySummary(node)), activityText(p, node.activity)); }
        return item;
      }));
    }).filter(Boolean));
    const notices = [...snapshot.inventory, snapshot.catalog, ...snapshot.clientResults].filter(value => value.status === 'rejected').map(value => problem(value.reason));
    if (!model.clients.length && snapshot.clientResults[0].status === 'fulfilled') notices.push(message('No authenticated client activity observed yet. Connect a client and send a request.'));
    if (snapshot.clientResults[0].value?.metadata?.truncated) notices.push(message('Some older client activity was omitted at the observation limit.', 'warning'));
    feedback.replaceChildren(...notices);
    if (graph) {
      void graph.update(model.elements).then(() => { if (p.current() && !graphFailed) graphStatus.textContent = ''; });
      graph.select(selected);
    }
  }

  const idle = () => !selected && !document.hidden && !list.contains(document.activeElement) && p.api.canChange();
  async function readSnapshot(automatic = false) {
    if (!p.current()) return false;
    refreshButton.disabled = true;
    try {
      const [inventory, catalog, clientResults] = await Promise.all([readInventory(p), Promise.allSettled([p.api.list('/providers/catalog?kind=all', { signal: p.scope.signal })]), readClients(p)]);
      if (!p.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      snapshot = { inventory, catalog: catalog[0], clientResults };
      updateModel();
      if (drawer && !drawer.editing) select(selected, true);
      freshness.textContent = `Updated ${date(new Date().toISOString())}. Refreshes every 15 seconds while idle.`;
      return ![...inventory, catalog[0], ...clientResults].some(result => result.status === 'rejected');
    } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); return false; }
    finally { refreshButton.disabled = !p.api.canLeave(); }
  }
  await read();
  if (!p.current() || !snapshot) return;
  select(selected);
  p.handleSelection?.(select);
  setListMode(false);
  const failed = () => {
    if (!p.current()) return;
    graphFailed = true;
    graphStatus.textContent = 'The network map is unavailable. Use the list to inspect and edit your nodes.';
    setListMode(true);
    view.disabled = true;
  };
  void topology(canvas, p.scope.signal, value => p.go('overview', value), failed).then(value => {
    if (!p.current() || !value) return;
    graph = value;
    graph.pause(live.paused);
    setListMode(listMode);
    updateModel();
  }).catch(failed);
  live.start();
}
