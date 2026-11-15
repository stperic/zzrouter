import { providerCategory } from './inventory.mjs';

export const rowsFrom = result => result?.status === 'fulfilled' ? result.value : [];
export const nodeHealth = node => ({ healthy: ['Healthy', 'good'], down: ['Down', 'bad'], degraded: ['Degraded', 'warn'] })[node?.health_status] || ['Unknown', ''];
export const nodeKey = name => `node/${name}`;
export const localProviderKey = (node, provider) => `local/${encodeURIComponent(node)}/${encodeURIComponent(provider)}`;
export const clientKey = row => [row.source, row.key_id, row.protocol, row.authentication].map(value => encodeURIComponent(value || '')).join(':');
export const clientLabel = row => row.name ? `Key: ${row.name}` : `Key ID: ${row.key_id}`;
export const protocolName = value => ({ 'management-mcp': 'Management MCP', 'inference-mcp': 'Inference MCP', 'management-api': 'Management API', 'inference-api': 'Inference API' })[value] || 'Protocol unknown';
export const clientState = row => row.active_requests > 0 ? ['Request in progress', 'good'] : ['No active requests', ''];

export function networkNodes(inventory) {
  const observed = inventory[0].status === 'fulfilled';
  const records = observed ? inventory[0].value : rowsFrom(inventory[1]).map(row => ({ name: row.node }));
  return [...new Map(records.filter(node => node.name).map(node => [node.name, node])).values()];
}

export function nodeSummary(inventory) {
  if (inventory[0].status !== 'fulfilled') return null;
  const nodes = networkNodes(inventory);
  return { total: nodes.length, healthy: nodes.filter(node => node.health_status === 'healthy').length,
    down: nodes.filter(node => node.health_status === 'down').length,
    unknown: nodes.filter(node => !['healthy', 'down', 'degraded'].includes(node.health_status)).length,
    degraded: nodes.filter(node => node.health_status === 'degraded').length };
}

export function localProviders(node, inventory) {
  const reports = (node.providers || []).filter(row => providerCategory(row) === 'local');
  const records = rowsFrom(inventory[1]).filter(row => row.node === node.name && providerCategory(row) === 'local');
  const names = [...new Set([...records.map(row => row.name), ...reports.map(row => row.key)])];
  return names.map(key => ({ key, report: reports.find(row => row.key === key), record: records.find(row => row.name === key) }));
}

export function cloudProviders(catalog, inventory) {
  const entries = rowsFrom(catalog).filter(entry => providerCategory(entry) === 'cloud');
  const records = rowsFrom(inventory[1]).filter(row => providerCategory(row) === 'cloud');
  const reported = networkNodes(inventory).flatMap(node => (node.providers || []).filter(row => providerCategory(row) === 'cloud').map(row => ({ ...row, node: node.name })));
  const names = new Set([...entries.filter(entry => entry.available).map(entry => entry.key), ...records.filter(row => row.enabled).map(row => row.name), ...reported.map(row => row.key)]);
  return [...names].map(key => ({ key, name: entries.find(entry => entry.key === key)?.name || reported.find(row => row.key === key)?.name || key,
    configured: entries.find(entry => entry.key === key)?.available,
    nodes: [...new Set([...records.filter(row => row.name === key && row.enabled).map(row => row.node), ...reported.filter(row => row.key === key).map(row => row.node)])] }));
}

export function hardwareLabel(node) {
  const names = node.gpu?.gpus?.map(gpu => gpu.name).filter(Boolean);
  return names?.length ? names.join(' · ') : node.gpu?.type || 'Hardware not reported';
}

export function memoryLabel(node) {
  const value = node.memory?.total_gb;
  return Number.isFinite(value) ? `${value} GiB${node.gpu?.unified_memory ? ' unified memory' : ' RAM'}` : 'Not reported';
}
