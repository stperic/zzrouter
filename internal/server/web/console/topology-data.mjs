import { networkNodes, nodeHealth, nodeKey, localProviderKey, clientKey, clientLabel, clientState, cloudProviders, rowsFrom, localProviders } from './network-data.mjs';
import { memorySummary, activityDeadline } from './observations.mjs';

const label = (name, status) => `${String(name).replaceAll('-', '-\u200b')}\n${status}`;
export const computeCard = { width: 280, height: 180 };

// Branch visibility is a projection. The complete inventory and stable selection
// identities remain available to the list, detail panel and deep links.
export function visibleTopology(elements, expanded) {
  const hidden = new Set(elements.filter(({ data }) => data.kind === 'provider' && !expanded.has(data.nodeName)).map(({ data }) => data.id));
  return elements.filter(({ data }) => !hidden.has(data.id) && !hidden.has(data.source) && !hidden.has(data.target));
}

// Visual roles have distinct IDs; their selection always uses the domain identity.
export function topologyData({ inventory, catalog, clientResults }, coordinatorName) {
  const nodes = networkNodes(inventory).sort((a, b) => a.name.localeCompare(b.name));
  const clients = clientResults[0].status === 'fulfilled' ? clientResults[0].value.data || [] : [];
  const clouds = cloudProviders(catalog, inventory).sort((a, b) => a.key.localeCompare(b.key));
  const coordinator = nodes.find(node => node.name === coordinatorName) || { name: coordinatorName };
  const elements = [];
  const providers = [];
  const add = (id, name, status, tone, kind, selection, parent) => elements.push({ data: { id, name, status, tone, kind, selection, parent, label: label(name, status) } });
  const edge = (source, target, tone = '') => elements.push({ data: { id: JSON.stringify([source, target]), source, target, tone } });
  const group = (id, name) => elements.push({ data: { id, name, label: name, kind: 'group' } });
  if (clients.length) group('clients', `CLIENTS · ${clients.length} observed`);
  if (nodes.length) group('compute', `COMPUTE NODES · ${nodes.length} machines`);
  if (clouds.length) group('clouds', 'CLOUD PROVIDERS');
  add('coordinator', coordinatorName, `Coordinator · ${nodeHealth(coordinator)[0]}`, nodeHealth(coordinator)[1], 'coordinator', nodeKey(coordinatorName));
  for (const row of clients) {
    const selection = `client/${clientKey(row)}`;
    add(selection, clientLabel(row), clientState(row)[0], clientState(row)[1], 'client', selection, 'clients');
    edge(selection, 'coordinator', row.active_requests > 0 ? 'good' : '');
  }
  for (const node of nodes) {
    const id = nodeKey(node.name);
    add(id, node.name, `${node.name === coordinatorName ? 'Coordinator + Worker · ' : ''}${nodeHealth(node)[0]}`, nodeHealth(node)[1], 'compute', id, 'compute');
    Object.assign(elements.at(-1).data, { label: node.name, caption: `${node.name === coordinatorName ? 'Worker · ' : ''}${nodeHealth(node)[0]}`,
      metrics: memorySummary(node), activityUntil: !node.activity?.stale ? activityDeadline(node.activity) : 0, active: node.activity?.state === 'active' });
    edge('coordinator', id, nodeHealth(node)[1]);
    for (const provider of localProviders(node, inventory)) {
      const providerID = localProviderKey(node.name, provider.key);
      providers.push({ selection: providerID, nodeName: node.name, providerKey: provider.key });
      if (provider.record?.enabled === false) continue;
      const status = provider.record?.enabled === true ? 'Enabled' : 'Unknown';
      add(providerID, provider.report?.name || provider.key, status, '', 'provider', providerID, 'compute');
      elements.at(-1).data.providerKey = provider.key;
      elements.at(-1).data.nodeName = node.name;
      edge(id, providerID);
    }
  }
  for (const entry of clouds) {
    const id = `cloud/${entry.key}`;
    add(id, entry.name, entry.configured === true ? 'Configured' : entry.configured === false ? 'Not configured here' : 'Configuration unknown', '', 'cloud', id, 'clouds');
    elements.at(-1).data.providerKey = entry.key;
    edge('coordinator', id);
  }
  return { elements, nodes, clients, clouds, providers, grants: rowsFrom(clientResults[1]) };
}

export function layoutInput(elements, bounds = new Map()) {
  const nodes = elements.filter(element => !element.data.source);
  const boxes = new Map(nodes.map(({ data }) => [data.id, data.kind === 'group'
    ? { id: data.id, children: [], layoutOptions: { 'elk.padding': '[top=30,left=24,bottom=16,right=24]' } }
    : { id: data.id, width: Math.max(data.kind === 'compute' ? computeCard.width : data.kind === 'provider' ? 110 : 180, Math.ceil(bounds.get(data.id)?.w || 0)),
      height: Math.max(data.kind === 'compute' ? computeCard.height : 124, Math.ceil(bounds.get(data.id)?.h || 0) + 16), compute: data.kind === 'compute' }]));
  const children = [];
  for (const { data } of nodes) (data.parent ? boxes.get(data.parent).children : children).push(boxes.get(data.id));
  return { id: 'root', children, edges: elements.filter(element => element.data.source).map(({ data }) => ({ id: data.id, sources: [data.source], targets: [data.target] })),
    layoutOptions: { 'elk.algorithm': 'layered', 'elk.direction': 'DOWN', 'elk.hierarchyHandling': 'INCLUDE_CHILDREN',
      'elk.spacing.nodeNode': '20', 'elk.layered.spacing.nodeNodeBetweenLayers': '32', 'elk.layered.considerModelOrder.strategy': 'NODES_AND_EDGES' } };
}

export function layoutPositions(graph, origin = { x: 0, y: 0 }, positions = new Map()) {
  for (const node of graph.children || []) {
    const offset = { x: origin.x + node.x, y: origin.y + node.y };
    if (node.children) layoutPositions(node, offset, positions);
    else positions.set(node.id, { x: offset.x + node.width / 2, y: offset.y + (node.compute ? node.height / 2 : 32) });
  }
  return positions;
}

export const topologySignature = (elements, layout) => JSON.stringify([
  elements.map(({ data }) => [data.id, data.parent, data.source, data.target]).sort((a, b) => a[0].localeCompare(b[0])),
  layout?.children.flatMap(node => node.children || [node]).map(node => [node.id, node.width, node.height]).sort((a, b) => a[0].localeCompare(b[0])),
]);
