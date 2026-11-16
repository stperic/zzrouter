export const modelKey = model => [model.node, model.assigned_app, model.source_repo, model.name, ...(model.source_id ? [model.source_id] : [])].map(value => encodeURIComponent(value || '')).join('/');
export const modelIdentity = model => Object.fromEntries([['name', model.name], ['node', model.node], ['provider', model.assigned_app], ['registry', model.source_repo], ['source_id', model.source_id]].filter(([, value]) => value));
export const modelQuery = model => new URLSearchParams(Object.fromEntries(Object.entries(modelIdentity(model)).map(([key, value]) => [key === 'name' ? 'model' : key, value])));
export function modelInferenceTarget(model) {
  const provider = model.assigned_app, address = model.name;
  return { model: provider && !address.toLowerCase().startsWith(provider.toLowerCase() + '/') ? `${provider}/${address}` : address,
    node: model.node && model.node !== 'cloud' ? model.node : undefined };
}
export const discoveryKey = item => [item.provider, item.api_id].map(encodeURIComponent).join('/');
export function discoveryIdentity(key) {
  const [source, ...name] = String(key || '').split('/');
  try { return { provider: decodeURIComponent(source), api_id: decodeURIComponent(name.join('/')) }; }
  catch { return { provider: '', api_id: '' }; }
}
export function bytes(value) {
  if (!Number.isFinite(Number(value)) || value === undefined || value === null) return 'Not reported';
  const number = Number(value);
  if (number >= 1024 ** 3) return `${(number / 1024 ** 3).toFixed(1)} GiB`;
  if (number >= 1024 ** 2) return `${(number / 1024 ** 2).toFixed(1)} MiB`;
  return `${number.toLocaleString()} bytes`;
}
export function searchPath({ provider, query = '', sort = '', tags = '', logic = 'AND', author = '', cursor = '', offset = 0 }) {
  const values = { type: 'models', provider, q: query, sort, tags, logic, author, order: 'desc', limit: '30', full: 'true', enrich: 'true' };
  if (cursor) values.cursor = cursor;
  else if (offset) values.offset = String(offset);
  return '/search?' + new URLSearchParams(Object.fromEntries(Object.entries(values).filter(([, value]) => value !== '')));
}
export function searchSelection(source, values) {
  return { ...values, provider: source.name, query: source.client_filter ? '' : values.query,
    sort: source.client_sorts?.includes(values.sort) ? '' : values.sort === 'newest' ? 'lastModified' : values.sort,
    viewQuery: source.client_filter ? values.query : '', viewSort: source.client_sorts?.includes(values.sort) ? values.sort : '' };
}
export function discoveryRows(results, { viewQuery = '', viewSort = '' } = {}) {
  const query = viewQuery.toLowerCase();
  const rows = results.filter(item => item.discovery && (!query || `${item.discovery.display_name} ${item.discovery.api_id}`.toLowerCase().includes(query)));
  const fields = { popular: 'downloads', downloads: 'downloads', likes: 'likes', trendingScore: 'trending_score', context: 'context_length' };
  if (fields[viewSort]) rows.sort((a, b) => (b.discovery[fields[viewSort]] || 0) - (a.discovery[fields[viewSort]] || 0));
  else if (viewSort === 'name') rows.sort((a, b) => (a.discovery.display_name || a.discovery.api_id).localeCompare(b.discovery.display_name || b.discovery.api_id));
  else if (['newest', 'lastModified', 'date', 'created', 'createdAt'].includes(viewSort)) rows.sort((a, b) => (Date.parse(b.discovery.modified_at || b.discovery.created_at) || 0) - (Date.parse(a.discovery.modified_at || a.discovery.created_at) || 0));
  else if (['price', 'price-desc'].includes(viewSort)) rows.sort((a, b) => {
    const price = item => item.price_prompt !== undefined && item.price_prompt !== '' && Number.isFinite(Number(item.price_prompt)) ? Number(item.price_prompt) : Infinity;
    const left = price(a.discovery), right = price(b.discovery);
    if (!Number.isFinite(left) || !Number.isFinite(right)) return Number.isFinite(left) ? -1 : Number.isFinite(right) ? 1 : 0;
    return viewSort === 'price' ? left - right : right - left;
  });
  return rows;
}
export function nextSearchPage(page) {
  if (!page?.pagination?.has_next) return undefined;
  const next = page.pagination.cursor?.next;
  // A cursor API must supply its continuation; an offset is not a replacement.
  if (page.pagination.cursor) return next ? { cursor: next } : undefined;
  return { offset: (page.pagination.offset || 0) + (page.pagination.limit || page.results?.length || 30) };
}
export const deploymentTerminal = value => ['completed', 'failed', 'skipped', 'cancelled', 'canceled'].includes(value);
