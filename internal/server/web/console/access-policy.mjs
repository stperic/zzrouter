import { el, text, field, input, select, button, actions, message, invalidField } from './dom.mjs';
import { providerCategory } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';

export function providerPolicy(record) {
  const policy = record?.provider_access;
  return policy?.mode === 'selected' ? { mode: 'selected', providers: [...(policy.providers || [])].sort() } : { mode: policy?.mode || 'all' };
}
export function accessLabel(record) {
  if (!record) return 'Team policy unavailable';
  const routes = routePolicy(record);
  if (routes.mode === 'none') return 'No inference';
  if (routes.mode === 'selected') return `${routes.routes.length} assigned route${routes.routes.length === 1 ? '' : 's'}`;
  const policy = providerPolicy(record);
  return policy.mode === 'none' ? 'No providers' : policy.mode === 'selected' ? `${policy.providers.length} selected provider${policy.providers.length === 1 ? '' : 's'}` : 'All providers';
}
export function policySummary(record, catalog = []) {
  const policy = providerPolicy(record);
  const routes = routePolicy(record);
  return el('div', { class: 'policy-summary' }, el('strong', {}, accessLabel(record)),
    routes.mode === 'selected' ? text(`Assigned routes only: ${routes.routes.join(', ')}`, 'muted') : routes.mode === 'direct' ? text('Direct models and routes', 'muted') : null,
    routes.mode !== 'direct' ? text(`Provider ceiling: ${policy.mode === 'all' ? 'All providers' : policy.mode === 'none' ? 'No providers' : 'Selected providers'}`, 'muted') : null,
    policy.mode === 'selected' ? el('div', { class: 'access-chips' }, policy.providers.map(key => providerName(key, catalog.find(item => item.key === key)?.name || key))) : null,
    text(record?.allowed_models?.length ? `Models: ${record.allowed_models.join(', ')}` : 'All models within the allowed providers', 'muted'));
}

export function providerFields(record, catalog, unavailable = false) {
  const policy = providerPolicy(record);
  const chosen = new Set(policy.providers || []);
  const mode = select('provider_mode', [['all', 'All providers'], ['selected', 'Selected providers'], ['none', 'No providers']], policy.mode);
  const query = input('provider_search', '', { type: 'search', placeholder: 'Search providers', 'aria-label': 'Search providers' });
  const options = el('div', { class: 'provider-options' });
  const selected = el('p', { class: 'selection-count', 'aria-live': 'polite' });
  const rows = catalog.filter(item => item.key && providerCategory(item) !== 'registries').map(item => ({ ...item, category: providerCategory(item) }));
  for (const key of chosen) if (!rows.some(item => item.key === key)) rows.push({ key, name: key, category: 'unavailable' });
  const render = () => {
    options.replaceChildren();
    selected.textContent = `${chosen.size} selected`;
    for (const [category, title] of [['local', 'Local providers'], ['cloud', 'Cloud providers'], ['unavailable', 'Unavailable in catalog']]) {
      const matches = rows.filter(item => item.category === category && `${item.key} ${item.name}`.toLowerCase().includes(query.value.toLowerCase()));
      if (!matches.length) continue;
      const group = el('fieldset', { class: 'provider-choice-group' }, el('legend', {}, title));
      for (const item of matches) {
        const checkbox = input('provider', item.key, { type: 'checkbox', checked: chosen.has(item.key) });
        checkbox.addEventListener('change', () => { checkbox.checked ? chosen.add(item.key) : chosen.delete(item.key); selected.textContent = `${chosen.size} selected`; });
        group.append(el('label', { class: 'provider-choice' }, checkbox, providerName(item.key, item.name || item.key), el('small', {}, category === 'unavailable' ? 'Saved permission' : item.available === false ? 'Not configured' : '')));
      }
      options.append(group);
    }
    if (!options.children.length) options.append(text('No matching providers.', 'muted'));
  };
  const choices = el('div', {}, query, selected, options);
  const update = () => { choices.hidden = mode.value !== 'selected'; };
  query.addEventListener('input', render);
  mode.addEventListener('change', update);
  render(); update();
  return {
    control: mode,
    root: el('section', { class: 'access-section' }, el('h3', {}, 'Provider access'), field('Allow requests to', mode),
      text('Applies across all compute nodes, including fallback routes. All providers includes providers added later.', 'muted'), unavailable ? message('Provider catalog unavailable. Saved selections are retained; reload to see other choices.', 'warning') : null, choices),
    read: () => {
      if (mode.value === 'selected' && !chosen.size) throw invalidField('Select at least one provider, or choose No providers.', mode);
      return mode.value === 'selected' ? { mode: 'selected', providers: [...chosen].sort() } : { mode: mode.value };
    },
  };
}

export function modelFields(record, models, groups, unavailable = false) {
  const chosen = new Set(record.allowed_models || []);
  const mode = select('model_mode', [['all', 'All models'], ['selected', 'Selected models or groups']], chosen.size ? 'selected' : 'all');
  const query = input('model_search', '', { type: 'search', placeholder: 'Search models or groups', 'aria-label': 'Search models or groups' });
  const manual = input('model_name', '', { placeholder: 'Exact model or group name', 'aria-label': 'Exact model or group name' });
  const options = el('div', { class: 'model-options' });
  const names = new Set([...chosen, ...models.map(row => row.name), ...groups.map(row => row.name)].filter(Boolean));
  const render = () => {
    options.replaceChildren();
    for (const name of [...names].sort().filter(name => name.toLowerCase().includes(query.value.toLowerCase()))) {
      const checkbox = input('model', name, { type: 'checkbox', checked: chosen.has(name) });
      checkbox.addEventListener('change', () => { checkbox.checked ? chosen.add(name) : chosen.delete(name); });
      options.append(el('label', { class: 'model-choice' }, checkbox, name));
    }
    if (!options.children.length) options.append(text('No matching models. Add an exact name below.', 'muted'));
  };
  const choices = el('div', {}, query, options, actions(manual, button('Add name', () => { const name = manual.value.trim(); if (name) { chosen.add(name); names.add(name); manual.value = ''; query.value = ''; render(); } })));
  const update = () => { choices.hidden = mode.value !== 'selected'; };
  mode.addEventListener('change', update); query.addEventListener('input', render); render(); update();
  return {
    control: mode,
    root: el('section', { class: 'access-section' }, el('h3', {}, 'Model access'), field('Within allowed providers', mode),
      text('Optional narrowing by exact model or group name. Group membership can change. This does not restrict which compute node serves a request.', 'muted'), unavailable ? message('Model choices are incomplete. Existing permissions remain selected; exact names can still be added.') : null, choices),
    read: () => {
      if (mode.value === 'selected' && !chosen.size) throw invalidField('Select at least one model, or choose All models. Use No providers to block all inference.', mode);
      return mode.value === 'selected' ? [...chosen].sort() : [];
    },
  };
}

export function routePolicy(record) {
  const policy = record?.route_access;
  return policy?.mode === 'selected' ? { mode: 'selected', routes: [...(policy.routes || [])].sort() } : { mode: policy?.mode || 'direct' };
}

export function routeFields(record, groups, unavailable = false) {
  const policy = routePolicy(record);
  const chosen = new Set(policy.routes || []);
  const mode = select('route_mode', [['direct', 'Direct models & routes'], ['selected', 'Assigned routes only'], ['none', 'No inference']], policy.mode);
  const search = input('assigned_route_search', '', { type: 'search', placeholder: 'Search routes', 'aria-label': 'Search assigned routes' });
  const options = el('div', { class: 'model-options' });
  const names = [...new Set([...groups.map(group => group.name), ...chosen])].sort();
  const render = () => {
    options.replaceChildren();
    for (const name of names.filter(value => value.toLowerCase().includes(search.value.toLowerCase()))) {
      const route = groups.find(group => group.name === name);
      const checkbox = input('assigned_route', name, { type: 'checkbox', checked: chosen.has(name) });
      checkbox.addEventListener('change', () => { checkbox.checked ? chosen.add(name) : chosen.delete(name); });
      const summary = route ? (route.replicas || []).map(replica => `${replica.provider} / ${replica.model}${replica.node ? ` on ${replica.node}` : ''}`).join(' · ') : 'Saved assignment. Route unavailable.';
      options.append(el('label', { class: 'route-choice' }, checkbox, el('span', {}, el('strong', {}, name), el('small', {}, summary))));
    }
    if (!options.children.length) options.append(text(names.length ? 'No matching routes.' : 'Create a route in Routes, then assign it here.', 'muted'));
  };
  const choices = el('div', {}, search, options);
  const update = () => { choices.hidden = mode.value !== 'selected'; };
  mode.addEventListener('change', update); search.addEventListener('input', render); render(); update();
  return {
    control: mode,
    root: el('section', { class: 'access-section' }, el('h3', {}, 'Route access'), field('Clients may use', mode),
      text('Assigned routes only blocks direct model requests and node pins. Provider permissions, model limits and budgets below still apply to every destination.', 'muted'),
      unavailable ? message('Route catalog unavailable. Saved assignments are retained.', 'warning') : null, choices),
    read: () => {
      if (mode.value === 'selected' && !chosen.size) throw invalidField('Select at least one route, or choose No inference.', mode);
      return mode.value === 'selected' ? { mode: 'selected', routes: [...chosen].sort() } : { mode: mode.value };
    },
  };
}
