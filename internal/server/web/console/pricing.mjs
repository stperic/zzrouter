import { el, text, input, field, button, leaveButton, actions, message, card, facts, date, problem, selectableRow, markSelection, formFeedback, editorActions } from './dom.mjs';
import { details, formSubmission, readAfterWrite } from './details.mjs';
import { Lifetime } from './api.mjs';

const rates = [['input_cost_per_1m', 'Input'], ['output_cost_per_1m', 'Output'], ['cache_read_cost_per_1m', 'Cached input']];
const price = value => value === undefined ? 'Not reported' : `$${Number(value).toLocaleString(undefined, { maximumFractionDigits: 8 })}`;
const identifier = record => `${encodeURIComponent(record.provider || '')}/${encodeURIComponent(record.model)}`;
const identity = value => {
  const separator = value.indexOf('/');
  return { provider: separator < 0 ? '' : decodeURIComponent(value.slice(0, separator)), model: decodeURIComponent(value.slice(separator + 1)) };
};
// Matches the documented case-insensitive override identity for editing
// evidence only. Effective price precedence always comes from the API.
export const priceOverrideKey = record => [record.provider?.trim(), record.model?.trim()].filter(Boolean).join('/').toLowerCase();
export function priceOverrideValues(record = {}) {
  return { provider: record.provider || '', model: record.model || '', input_cost_per_1m: record.input_cost_per_1m || 0,
    output_cost_per_1m: record.output_cost_per_1m || 0, cache_read_cost_per_1m: record.cache_read_cost_per_1m || 0, note: record.note || '' };
}
export function priceOverrideChanged(before, fresh) {
  return !fresh || JSON.stringify(priceOverrideValues(before)) !== JSON.stringify(priceOverrideValues(fresh));
}
const readOverrides = async (p, signal = p.scope.signal) => (await p.api.data('/pricing/overrides', { signal })).overrides;

export function priceOverrideForm(p, record, { seed = {}, cancel = p.close, saved, reload }) {
  const before = priceOverrideValues(record || seed);
  const form = el('form', { class: 'entity-editor pricing-editor' });
  const draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const feedback = el('div', { 'aria-live': 'polite' });
  const provider = input('price_provider', before.provider, { readOnly: !!record, placeholder: 'Optional exact provider name' });
  const model = input('price_model', before.model, { required: true, readOnly: !!record, placeholder: 'Exact model name or *' });
  const note = el('textarea', { name: 'price_note', rows: 3 }, before.note);
  const fields = { provider, model, note };
  controls.append(field('Provider', provider, 'Leave blank to apply to this model across providers.'), field('Model', model, 'Use * with a provider to cover all of its models.'),
    text('Rates are USD per 1 million tokens. These prices estimate usage cost; team budgets set spending limits.', 'muted'));
  const rateFields = el('div', { class: 'form-grid' });
  for (const [key, label] of rates) {
    fields[key] = input(`price_${key}`, record || seed[key] !== undefined ? before[key] : '', { type: 'number', min: 0, step: 'any', required: true });
    rateFields.append(field(`${label} ($ / 1M tokens)`, fields[key]));
  }
  controls.append(rateFields, field('Note', note), text('Zero is a deliberate rate, including for local hardware or subscription billing. Existing cached-input pricing is retained when you edit other fields.', 'muted'));
  const validation = formFeedback(form, feedback, path => fields[path]);
  const save = el('button', { type: 'submit', class: 'primary' }, record ? 'Save changes' : 'Create override');
  const submission = formSubmission(p, controls, save, draft);
  const write = async method => {
    const body = { provider: provider.value.trim(), model: model.value.trim(), note: note.value,
      ...Object.fromEntries(rates.map(([key]) => [key, Number(fields[key].value)])) };
    if (method !== 'DELETE' && record && !priceOverrideChanged(record, body)) { feedback.replaceChildren(message('No fields changed.')); return; }
    await submission.run(async () => {
      feedback.replaceChildren(message('Checking current pricing…'));
      const records = await readOverrides(p);
      if (!p.current()) return;
      const fresh = records.find(item => priceOverrideKey(item) === priceOverrideKey(body));
      if (record ? priceOverrideChanged(record, fresh) : !!fresh) {
        submission.block();
        throw new Error(record ? 'This override changed in another session. Inspect current pricing before editing.' : 'An override already exists for this provider and model. Inspect it before editing.');
      }
      return p.api.data(`/pricing/overrides${method === 'DELETE' ? `?${new URLSearchParams({ provider: body.provider, model: body.model })}` : ''}`,
        { method, body: method === 'DELETE' ? undefined : body, signal: p.scope.signal });
    }, () => readAfterWrite(p, () => saved(body, method === 'DELETE'), method === 'DELETE' ? 'Override deleted. The API will use the remaining price configuration.' : 'Price override saved.'), (error, blocked) => {
      validation.error(error);
      if (blocked) feedback.append(leaveButton('Inspect current pricing', () => { draft?.clean(); return reload(body); }, p.api));
    });
  };
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (form.reportValidity()) await write('POST');
  });
  if (record) controls.append(el('section', { class: 'access-section' }, el('h3', {}, 'Delete override'),
    text('Deletion restores any remaining matching price. It does not remove the model.', 'muted'),
    button('Delete override', async () => { if (confirm(`Delete the price override for ${record.model}?`)) await write('DELETE'); }, 'danger')));
  form.append(feedback, controls, text('Current values are read again before saving. The API does not lock concurrent editors, so another session can still change this override during the save.', 'muted'),
    editorActions(save, cancel, p.api, draft));
  return form;
}

export async function pricing(p, selected = '', mode = '') {
  const listing = el('div', { class: 'entity-list pricing-list' }, message('Loading pricing…'));
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  p.root.append(workspace);
  let status, records;
  try {
    [status, records] = await Promise.all([p.api.data('/pricing/status', { signal: p.scope.signal }), readOverrides(p)]);
  } catch (error) {
    if (p.current()) listing.replaceChildren(card('Pricing', problem(error), leaveButton('Retry pricing', () => p.reload(), p.api)));
    return;
  }
  if (!p.current()) return;
  let tab = mode === 'catalog' ? 'catalog' : 'overrides';
  let drawer;
  const open = (record, nextMode) => p.go('settings', `pricing/${identifier(record)}`, nextMode);
  const selectPrice = async (id = '', nextMode = '') => {
    drawer?.dispose(!id && nextMode !== 'create'); drawer = undefined;
    selected = id; mode = nextMode; markSelection(listing, id);
    if (!id && mode !== 'create') return;
    const owner = details({ ...p, root: workspace }, mode === 'create' ? 'Create price override' : 'Price details', () => p.go('settings', 'pricing'));
    drawer = owner;
    owner.root.append(message('Loading price details…'));
    const reload = async (target, deleted = false, notice) => {
      const revision = owner.panel.revision;
      const fresh = await Promise.all([p.api.data('/pricing/status', { signal: owner.panel.signal }), readOverrides(p, owner.panel.signal)]);
      if (!owner.panel.current() || owner.panel.revision !== revision || !p.api.canLeave()) return;
      [status, records] = fresh; render();
      if (deleted) {
        p.selectSection?.('pricing'); await selectPrice(); listing.prepend(message(notice || 'Price override deleted.')); return;
      }
      const targetID = identifier(target);
      p.selectSection?.(`pricing/${targetID}`);
      await selectPrice(targetID);
    };
    try {
      const target = id ? identity(id) : {};
      let record;
      if (mode === 'catalog') record = await p.api.data(`/pricing?${new URLSearchParams(target)}`, { signal: owner.scope.signal });
      else if (mode !== 'create') {
        const fresh = await readOverrides(owner);
        record = fresh.find(item => priceOverrideKey(item) === priceOverrideKey(target));
        if (!record) throw Object.assign(new Error('This override no longer exists. Refresh the price list.'), { status: 404 });
      }
      if (!owner.current()) return;
      const edit = () => {
        const editor = owner.panel.view(record ? 'Edit price override' : 'Create price override');
        editor.root.append(el('h2', {}, record?.model || 'Create a price override'), priceOverrideForm(editor, record, {
          seed: target, cancel: record ? show : () => p.go('settings', 'pricing'), saved: (body, deleted) => reload(body, deleted), reload: body => reload(body),
        }));
        p.syncActivity?.();
      };
      const show = () => {
        const reader = owner.panel.view(mode === 'catalog' ? 'Catalog price' : 'Price override');
        const existing = records.find(item => priceOverrideKey(item) === priceOverrideKey(target));
        const action = mode === 'catalog'
          ? leaveButton(existing ? 'View override' : 'Create override', () => open(existing || target, existing ? undefined : 'create'), p.api, 'primary')
          : leaveButton('Edit', edit, p.api, 'primary');
        priceDetails(reader, record, mode === 'catalog', action, () => open(record, 'catalog'));
        p.syncActivity?.();
      };
      if (mode === 'create') edit(); else show();
    } catch (error) {
      if (owner.current()) owner.root.replaceChildren(problem(error), leaveButton('Reload details', () => selectPrice(id, nextMode), p.api));
    }
  };
  const overrideSearch = input('override_search', '', { type: 'search', placeholder: 'Search overrides', 'aria-label': 'Search price overrides' });
  const result = el('div');
  let page = 0;
  const renderOverrides = () => {
    const query = overrideSearch.value.trim().toLowerCase();
    const filtered = records.filter(item => `${item.provider || ''} ${item.model} ${item.note || ''}`.toLowerCase().includes(query));
    page = Math.max(0, Math.min(page, Math.ceil(filtered.length / 20) - 1));
    const body = el('tbody');
    for (const row of filtered.slice(page * 20, page * 20 + 20)) {
      body.append(selectableRow(identifier(row), selected, () => open(row),
        el('td', {}, leaveButton(row.model, () => open(row), p.api, 'link entity-title'), text(row.provider || 'Any provider', 'muted')),
        ...rates.map(([key]) => el('td', {}, price(row[key] ?? 0))), el('td', {}, row.note || '')));
    }
    const previous = button('Previous', () => { page--; renderOverrides(); }); previous.disabled = page === 0;
    const next = button('Next', () => { page++; renderOverrides(); }); next.disabled = (page + 1) * 20 >= filtered.length;
    result.replaceChildren(filtered.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Model', 'Input', 'Output', 'Cached input', 'Note'].map(label => el('th', { scope: 'col' }, label)))), body)) : message(query ? 'No matching overrides.' : 'No price overrides. Add one for custom pricing or a provider without per-token billing.'),
      actions(text(`${filtered.length} overrides · USD per 1M tokens`, 'muted'), filtered.length > 20 ? [previous, next] : null));
  };
  overrideSearch.addEventListener('input', () => { page = 0; renderOverrides(); });
  const catalog = catalogSearch(p, open);
  const render = () => {
    const tabs = el('div', { class: 'tabs', 'aria-label': 'Pricing views' }, [['overrides', 'Overrides'], ['catalog', 'Catalog'], ['unpriced', `Unpriced (${status.unpriced_count || 0})`]].map(([key, label]) => {
      const control = button(label, () => { tab = key; render(); }, key === tab ? 'active' : 'secondary');
      if (key === tab) control.setAttribute('aria-current', 'page'); return control;
    }));
    listing.replaceChildren(card('Pricing', actions(leaveButton('Refresh', refresh, p.api), leaveButton('+ Create override', () => p.go('settings', 'pricing', 'create'), p.api, 'primary')),
      facts([['Catalog models', status.model_count ?? 'Not reported'], ['Last fetched', status.fetched_at ? date(status.fetched_at) : 'Not fetched']]), tabs,
      tab === 'overrides' ? [overrideSearch, result] : tab === 'catalog' ? catalog : unpricedList(status.unpriced || {}, () => p.go('settings', 'pricing', 'create'))));
    if (tab === 'overrides') renderOverrides();
  };
  const refresh = async () => {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    const active = drawer; const revision = active?.panel.revision;
    try {
      const fresh = await Promise.all([p.api.data('/pricing/status', { signal: p.scope.signal }), readOverrides(p)]);
      if (!p.current()) return;
      [status, records] = fresh; render();
      if (active === drawer && active?.panel.revision === revision && p.api.canLeave()) await selectPrice(selected, mode);
    } catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  p.handleSelection?.((next = 'pricing', nextMode) => selectPrice(next.slice('pricing'.length).replace(/^\//, ''), nextMode), next => next === 'pricing' || next?.startsWith('pricing/'));
  render(); await selectPrice(selected, mode);
}

function priceDetails(p, record, catalog, action, effective) {
  p.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, record.model), action),
    text(record.provider || 'Any provider', 'muted'), text('USD per 1 million tokens', 'muted'),
    facts(rates.map(([key, label]) => [label, price(record[key])])), record.note ? text(record.note) : '');
  if (catalog) p.root.append(facts([
    ['Cache write ($ / 1M)', price(record.cache_write_cost_per_1m)], ['Reasoning output ($ / 1M)', price(record.output_cost_per_reasoning_1m)],
    ['Input token limit', record.max_input_tokens ?? 'Not reported'], ['Output token limit', record.max_output_tokens ?? 'Not reported'], ['Mode', record.mode || 'Not reported'],
  ]), text('These are the effective rates returned by the pricing API, including matching overrides.', 'muted'));
  else p.root.append(facts([['Billing', record.billed_per_token ? 'Per token' : 'No per-token charge declared'], ['Coverage', record.applies_to_all_models ? 'All models for this provider' : 'This model'], ['Updated', record.updated_at ? date(record.updated_at) : 'Not reported'], ['Updated by', record.updated_by || 'Not reported']]),
    leaveButton('View effective price', effective, p.api));
}

function catalogSearch(p, open) {
  const form = el('form', { class: 'pricing-catalog' });
  const query = input('price_query', '', { type: 'search', placeholder: 'Model name', 'aria-label': 'Search pricing catalog' });
  const provider = input('catalog_provider', '', { placeholder: 'Optional provider name', 'aria-label': 'Filter pricing by provider' });
  const submit = el('button', { type: 'submit', class: 'secondary' }, 'Search catalog');
  const result = el('div', { 'aria-live': 'polite' }, message('Search by model or provider. Rates are USD per 1 million tokens.'));
  const reads = new Lifetime();
  p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  form.append(el('div', { class: 'form-grid' }, field('Model search', query), field('Provider', provider)), submit, result);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!query.value.trim() && !provider.value.trim()) { result.replaceChildren(message('Enter a model search or a provider.')); return; }
    const scope = reads.next();
    submit.disabled = true; result.replaceChildren(message('Searching prices…'));
    try {
      const response = await p.api.request(`/zzrouter/v1/pricing?${new URLSearchParams({ q: query.value.trim(), provider: provider.value.trim() })}`, { signal: AbortSignal.any([p.scope.signal, scope.signal]) });
      if (!p.current() || !scope.current()) return;
      const rows = response.data || [];
      const body = el('tbody', {}, rows.map(row => selectableRow(identifier(row), '', () => open(row, 'catalog'),
        el('td', {}, leaveButton(row.model, () => open(row, 'catalog'), p.api, 'link entity-title'), text(row.provider || 'Unspecified provider', 'muted')),
        el('td', {}, price(row.input_cost_per_1m)), el('td', {}, price(row.output_cost_per_1m)))));
      result.replaceChildren(rows.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Model', 'Input', 'Output'].map(label => el('th', { scope: 'col' }, label)))), body)) : message('No matching prices.'),
        text(`${rows.length} of ${response.total ?? rows.length} results · USD per 1M tokens`, 'muted'),
        response.has_more ? message('The API returns up to 50 matches. Narrow the search to see more specific results.') : '');
    } catch (error) { if (p.current() && scope.current()) result.replaceChildren(problem(error)); }
    finally { if (p.current() && scope.current()) submit.disabled = false; }
  });
  return form;
}

function unpricedList(observations, create) {
  const rows = Object.entries(observations).sort(([a], [b]) => a.localeCompare(b));
  return el('section', {}, text('Observed requests without a known price, including refused budget admissions. Counts do not prove successful inference.', 'muted'),
    rows.length ? [el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, el('th', { scope: 'col' }, 'Observed provider/model identifier'), el('th', { scope: 'col' }, 'Observations'))),
      el('tbody', {}, rows.map(([key, count]) => el('tr', {}, el('td', {}, key), el('td', {}, count)))))),
    button('Create a price override', create), text('Use the exact provider and model when creating an override. The combined observed identifier may also contain slashes in the model name.', 'muted')]
      : message('No unpriced observations reported.'));
}
