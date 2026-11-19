import { el, text, leaveButton, badge, actions, message, card, facts, problem, date } from './dom.mjs';
import { observeActivity } from './observations.mjs';
import { providerDiagnostics } from './diagnostics.mjs';

export function providerCategory(entry) {
  if (entry.kind === 'registry') return 'registries';
  return entry.is_cloud || entry.cloud || entry.kind === 'cloud' ? 'cloud' : 'local';
}

export async function readInventory(p) {
  const startedAt = performance.now();
  const results = await Promise.allSettled(['/nodes', '/providers'].map(path => p.api.list(path, { signal: p.scope.signal })));
  for (const node of results[0].value || []) {
    observeActivity(node.activity, startedAt);
    for (const value of Object.values(node.provider_activity || {})) observeActivity(value, startedAt);
  }
  return results;
}

const rowsFrom = result => result.status === 'fulfilled' ? result.value : [];
const stale = node => node.health_status !== 'healthy';
const health = node => ({ healthy: ['Healthy', 'good'], down: ['Down', 'bad'], degraded: ['Degraded', 'warn'] })[node.health_status] || ['Unknown', ''];
const serviceState = provider => provider?.running === false ? 'Service not running' : provider?.running === true ? 'Service running' : '';

export function providerObservationSummary(entry, inventory) {
  if (providerCategory(entry) === 'cloud') return '';
  if (!inventory) return entry.description || 'Model registry connection';
  const [nodes, providers] = inventory;
  const records = rowsFrom(providers).filter(row => row.name === entry.key);
  const installed = new Set(records.filter(row => row.installed).map(row => row.node));
  const names = new Set([...installed, ...records.filter(row => row.enabled).map(row => row.node),
    ...rowsFrom(nodes).filter(node => node.providers?.some(row => row.key === entry.key)).map(node => node.name)].filter(Boolean));
  const enabled = new Set(records.filter(row => row.enabled).map(row => row.node)).size;
  const down = rowsFrom(nodes).filter(node => names.has(node.name) && node.health_status === 'down').length;
  return [names.size ? `${names.size} reported ${names.size === 1 ? 'node' : 'nodes'}` :
    nodes.status === 'fulfilled' && providers.status === 'fulfilled' ? 'Setup required' : 'No provider instances confirmed',
    providers.status === 'fulfilled' ? `${enabled} enabled` : 'Enablement unavailable', down ? `${down} ${down === 1 ? 'node' : 'nodes'} down` : '',
    nodes.status === 'rejected' ? 'Node inventory incomplete' : ''].filter(Boolean).join(' · ');
}

export function nodeInventory(p, [nodes, providers], entry) {
  const root = el('div', { class: 'inventory' });
  if (nodes.status === 'rejected') root.append(card('Node observations unavailable', problem(nodes.reason)));
  if (providers.status === 'rejected') root.append(message('Provider installation and enablement could not be read. Node reports are shown where available.', 'warning'), problem(providers.reason));
  const observed = nodes.status === 'fulfilled';
  const records = rowsFrom(providers).filter(row => providerCategory(row) === 'local');
  const knownNodes = observed ? nodes.value : [...new Set(records.map(row => row.node).filter(Boolean))].map(name => ({ name }));
  const cards = el('div', { class: entry ? 'provider-instances' : 'cards' });
  for (const node of knownNodes) {
    const reported = node.providers?.filter(row => providerCategory(row) === 'local' && (!entry || row.key === entry.key));
    const assigned = records.filter(row => row.node === node.name && (entry ? row.name === entry.key : row.enabled));
    const names = new Set([...assigned.map(row => row.name), ...(reported || []).map(row => row.key)]);
    const lastSeen = observed ? (node.last_seen_at ? date(node.last_seen_at) : 'Not reported') : 'Unavailable';
    const platform = observed ? [node.os, node.version].filter(Boolean).join(' · ') || 'Not reported' : 'Unavailable';
    const observations = facts([['Last seen', lastSeen], ['OS / Router version', platform]]);
    const body = card(node.name, actions(badge(observed ? node.cluster_role || 'Role unknown' : 'Role unavailable'), badge(...(observed ? health(node) : ['Unavailable', '']))),
      entry ? el('details', {}, el('summary', {}, 'Node observations'), observations) : observations);
    if (stale(node) && names.size) body.append(text('Last reported provider state.', 'muted'));
    if (!observed) body.append(message('Node observations unavailable.'));
    else if (providers.status === 'rejected' && !names.size) body.append(message('Provider status unavailable.'));
    else if (!Array.isArray(node.providers)) body.append(message('Provider status has not been reported by this node.'));
    else if (!names.size) body.append(text(entry ? 'This provider has not been reported by this node.' : 'No enabled local providers reported.', 'muted'));
    const chips = el('ul', { class: 'provider-chips', role: 'list', 'aria-label': 'Local providers' });
    for (const name of names) {
      const record = records.find(row => row.node === node.name && row.name === name);
      const routed = reported?.find(row => row.key === name);
      const service = serviceState(routed);
      if (!entry) {
        let label = routed?.name || name;
        if (record?.enabled === false) label += ' (disabled)';
        else if (!record) label += ' (enablement unavailable)';
        if (service) label += ` (${service.toLowerCase()})`;
        chips.append(el('li', {}, badge(label, `provider-chip ${routed?.running === false ? 'warn' : ''}`)));
        continue;
      }
      const row = el('div', { class: 'inventory-row' }, actions(
        record ? badge(record.enabled ? 'Enabled' : 'Disabled') : badge('Enablement unavailable'),
        record ? badge(record.installed ? 'Installed' : 'Not installed') : badge('Installation unavailable'),
        service ? badge(service, routed.running ? 'good' : 'warn') : null,
      ));
      body.append(row);
      body.append(el('details', {}, el('summary', {}, 'Lifecycle details'), providerDiagnostics(p, record || routed, node.name)));
    }
    if (!entry) {
      if (names.size) body.append(chips);
      body.append(actions(leaveButton('View local providers', () => p.go('providers', 'local'), p.api, 'link')));
    }
    if (entry) body.append(leaveButton('View node', () => p.go('nodes', `node/${node.name}`), p.api, 'link'));
    cards.append(body);
  }
  if (!knownNodes.length && nodes.status === 'fulfilled') root.append(message('No known nodes were returned.'));
  root.append(cards);
  return root;
}
