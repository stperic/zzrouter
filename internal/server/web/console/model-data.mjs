export const modelKey = model => [model.node, model.assigned_app, model.source_repo, model.name, ...(model.source_id ? [model.source_id] : [])].map(value => encodeURIComponent(value || '')).join('/');
export const modelIdentity = model => Object.fromEntries([['name', model.name], ['node', model.node], ['provider', model.assigned_app], ['registry', model.source_repo], ['source_id', model.source_id]].filter(([, value]) => value));
export const modelQuery = model => new URLSearchParams(Object.fromEntries(Object.entries(modelIdentity(model)).map(([key, value]) => [key === 'name' ? 'model' : key, value])));
export function modelInferenceTarget(model) {
  const provider = model.assigned_app, address = model.name;
  return { model: provider && !address.toLowerCase().startsWith(provider.toLowerCase() + '/') ? `${provider}/${address}` : address,
    node: model.node && model.node !== 'cloud' ? model.node : undefined };
}
export const discoveryKey = item => [item.provider, item.api_id].map(encodeURIComponent).join('/');
export const discoveryName = item => item.display_name || item.api_id;
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
export function searchPath({ provider, query = '', sort = '', order = 'desc', tags = '', logic = 'AND', author = '', cursor = '', offset = 0 }) {
  const values = { type: 'models', provider, q: query, sort, tags, logic, author, order, limit: '30', full: 'true', enrich: 'true' };
  if (cursor) values.cursor = cursor;
  else if (offset) values.offset = String(offset);
  return '/search?' + new URLSearchParams(Object.fromEntries(Object.entries(values).filter(([, value]) => value !== '')));
}
export function searchSelection(source, values) {
  if (source.name === 'ollama') {
    // Discover uses the coordinator's Ollama catalog search, not the CLI's page-level hints.
    return { provider: source.name, query: values.query, sort: values.sort, order: values.sort === 'name' ? 'asc' : 'desc' };
  }
  const nativeCloudSort = source.is_cloud && ['name', 'newest'].includes(values.sort);
  const clientSort = !nativeCloudSort && source.client_sorts?.includes(values.sort);
  const clientFilter = !source.is_cloud && source.client_filter;
  const nativeSort = values.sort === 'newest' ? (source.is_cloud ? 'date' : 'lastModified') : values.sort;
  return { ...values, provider: source.name, query: clientFilter ? '' : values.query,
    sort: clientSort ? '' : nativeSort, ...(nativeCloudSort && values.sort === 'name' ? { order: 'asc' } : {}),
    viewQuery: clientFilter ? values.query : '', viewSort: clientSort ? values.sort : '' };
}
export function discoveryRows(results, { viewQuery = '', viewSort = '' } = {}) {
  const query = viewQuery.toLowerCase();
  const seen = new Set();
  const rows = results.filter(({ discovery }) => {
    if (!discovery || query && !`${discoveryName(discovery)} ${discovery.api_id}`.toLowerCase().includes(query)) return false;
    const key = discoveryKey(discovery);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  const fields = { popular: 'downloads', downloads: 'downloads', likes: 'likes', trendingScore: 'trending_score', context: 'context_length' };
  if (fields[viewSort]) rows.sort((a, b) => (b.discovery[fields[viewSort]] || 0) - (a.discovery[fields[viewSort]] || 0));
  else if (viewSort === 'name') rows.sort((a, b) => discoveryName(a.discovery).localeCompare(discoveryName(b.discovery)));
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
