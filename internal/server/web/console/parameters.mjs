import { el, text, input, select, field, button, leaveButton, actions, message, problem, formFeedback, editorActions, clearSecrets } from './dom.mjs';
import { readAfterWrite } from './details.mjs';

const base = provider => `/zzrouter/v1/providers/${encodeURIComponent(provider)}`;
const scopes = [['defaults', 'Provider defaults'], ['model', 'Model defaults'], ['node', 'Compute node'], ['node-model', 'Model on a compute node']];
const has = (object, key) => Object.hasOwn(object || {}, key);

export function parameterQuery({ scope, node = '', model = '', endpoint = '' }) {
  const query = new URLSearchParams({ scope });
  if (scope === 'node' || scope === 'node-model') query.set('node', node);
  if (scope === 'model' || scope === 'node-model') query.set('model', model);
  if (endpoint) query.set('endpoint', endpoint);
  return query.toString();
}

// The API owns validation. Invalid numeric strings stay strings so its schema
// validator can explain the error; unsafe JS integers must never be rounded.
export function typedParameterValue(value, schema = {}) {
  if (value === null || value === schema.auto) return value;
  if (schema.type === 'bool' && ['true', 'false'].includes(value)) return value === 'true';
  if (schema.type === 'int' && /^[+-]?\d+$/.test(value) && Number.isSafeInteger(Number(value))) return Number(value);
  if (['float', 'number'].includes(schema.type) && value.trim() && Number.isFinite(Number(value))) return Number(value);
  return value;
}

export function parameterPatch(selection, changes, schema) {
  const leaf = {};
  for (const group of ['parameters', 'environment']) {
    if (!Object.keys(changes[group] || {}).length) continue;
    leaf[group] = Object.fromEntries(Object.entries(changes[group]).map(([key, value]) => [key, typedParameterValue(value, schema[group]?.[key])]));
  }
  const cell = selection.endpoint ? { endpoints: { [selection.endpoint]: leaf } } : leaf;
  switch (selection.scope) {
    case 'defaults': return { defaults: cell };
    case 'model': return { models: { [selection.model]: cell } };
    case 'node': return { nodes: { [selection.node]: cell } };
    case 'node-model': return { nodes: { [selection.node]: { models: { [selection.model]: cell } } } };
    default: throw new Error('Choose a configuration scope.');
  }
}

// Scoped GET keys use the server's canonical flag identity; JS does not infer
// aliases. Compare only edited leaves, retaining unrelated concurrent changes.
// Presence differs from a literal empty string.
export function parameterConflicts(before, fresh, changes) {
  return ['parameters', 'environment'].flatMap(group => Object.keys(changes[group] || {}).filter(key =>
    has(before[group], key) !== has(fresh[group], key) || before[group]?.[key] !== fresh[group]?.[key]
  ).map(key => `${group}.${key}`));
}

function valueControl(name, value, shape, sensitive) {
  if (sensitive) return input(name, '', { type: 'password', 'data-sensitive': true, placeholder: value === undefined ? 'Enter a value' : 'Saved value hidden' });
  let choices = shape.enum || (shape.type === 'bool' ? ['true', 'false'] : undefined);
  if (choices) {
    choices = [...new Set([...(value === undefined ? [''] : [value]), ...choices, ...(shape.auto ? [shape.auto] : [])])];
    return select(name, choices.map(item => [item, item === '' ? 'Choose a value' : item]), value);
  }
  const numeric = ['int', 'float', 'number'].includes(shape.type) && !shape.auto;
  return input(name, value ?? '', { type: numeric ? 'number' : 'text', step: numeric ? shape.type === 'int' ? 1 : 'any' : undefined,
    min: numeric ? shape.min : undefined, max: numeric ? shape.max : undefined,
    placeholder: shape.auto ? `Value or ${shape.auto}` : shape.type === 'asset' ? 'Configured asset name' : '' });
}

export async function parameterSettings(p, { provider, node = '', model = '', back = p.close }) {
  let selection = { scope: node ? model ? 'node-model' : 'node' : model ? 'model' : 'defaults', node, model, endpoint: '' };
  const load = async (next = selection, notice, runs) => {
    selection = next;
    const owner = p.panel.view('Runtime settings');
    owner.root.append(message('Loading runtime settings…'));
    const query = parameterQuery(selection);
    const effectiveQuery = new URLSearchParams();
    if (selection.node) effectiveQuery.set('node', selection.node);
    if (selection.model) effectiveQuery.set('model', selection.model);
    if (selection.endpoint) effectiveQuery.set('endpoint', selection.endpoint);
    const reads = await Promise.allSettled([
      owner.api.request(`${base(provider)}/schema`, { signal: owner.scope.signal }),
      owner.api.request(`${base(provider)}/parameters?${query}`, { signal: owner.scope.signal }),
      owner.api.request(`${base(provider)}/resolved?${effectiveQuery}`, { signal: owner.scope.signal }),
    ]);
    if (!owner.current()) return;
    const failure = reads.slice(0, 2).find(result => result.status === 'rejected');
    if (failure) {
      owner.root.replaceChildren(problem(failure.reason), actions(leaveButton('Reload settings', () => load(selection), owner.api), leaveButton('Back', back, owner.api)));
      return;
    }
    const schema = reads[0].value;
    const before = reads[1].value;
    const effective = reads[2].status === 'fulfilled' ? reads[2].value : null;
    const shapes = { parameters: { ...schema.parameters, ...schema.endpoints?.[selection.endpoint]?.parameters }, environment: schema.environment || {} };
    const form = el('form', { class: 'entity-editor parameter-editor' });
    const draft = owner.trackDraft(form);
    const controls = el('fieldset', { 'data-protected-actions': true });
    const feedback = el('div', { 'aria-live': 'polite' });
    const changes = { parameters: Object.create(null), environment: Object.create(null) };
    const fields = new Map();
    const entries = [];
    let blocked = false;
    const validation = formFeedback(form, feedback, path => [...fields].find(([key]) => path?.endsWith(`.${key}`))?.[1]);
    const count = el('span', { class: 'muted', role: 'status' });
    const changed = () => {
      const n = Object.keys(changes.parameters).length + Object.keys(changes.environment).length;
      count.textContent = `${n} ${n === 1 ? 'change' : 'changes'}`;
      if (n) draft.touch(); else draft.clean();
    };
    const update = (group, key, value) => {
      if (value === null ? !has(before[group], key) : has(before[group], key) && before[group][key] === value) delete changes[group][key];
      else changes[group][key] = value;
      changed();
    };
    const scope = select('parameter_scope', scopes, selection.scope);
    const nodeField = input('parameter_node', selection.node, { placeholder: 'Exact node name' });
    const modelField = input('parameter_model', selection.model, { placeholder: 'Exact model name or configured pattern' });
    const endpoint = select('parameter_endpoint', [['', 'All endpoints'], ...(before.available_endpoints || []).map(key => [key, key])], selection.endpoint);
    const nodeWrap = field('Compute node', nodeField);
    const modelWrap = field('Model', modelField);
    const scopeChanged = () => {
      nodeWrap.hidden = !['node', 'node-model'].includes(scope.value);
      modelWrap.hidden = !['model', 'node-model'].includes(scope.value);
      nodeField.required = !nodeWrap.hidden; modelField.required = !modelWrap.hidden;
    };
    scope.addEventListener('change', scopeChanged); scopeChanged();
    const scopeForm = el('form', { class: 'parameter-context' }, field('Edit scope', scope), nodeWrap, modelWrap, field('Endpoint', endpoint), el('button', { type: 'submit', class: 'secondary', 'data-leave-view': true }, 'View scope'));
    scopeForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (!scopeForm.reportValidity() || !draft.discard()) return;
      draft.clean();
      await load({ scope: scope.value, node: nodeWrap.hidden ? '' : nodeField.value.trim(), model: modelWrap.hidden ? '' : modelField.value.trim(), endpoint: endpoint.value });
    });
    const search = input('parameter_search', '', { type: 'search', placeholder: 'Search settings or environment names', 'aria-label': 'Search runtime settings' });
    const parameterRows = el('div', { class: 'parameter-fields' });
    const environmentRows = el('div', { class: 'parameter-fields' });
    const filter = () => {
      const query = search.value.trim().toLowerCase();
      for (const entry of entries) entry.root.hidden = !entry.search.includes(query);
    };
    search.addEventListener('input', filter);
    const addRow = (group, key) => {
      const shape = shapes[group][key] || {};
      const present = has(before[group], key);
      const sensitive = group === 'environment';
      let value = present ? before[group][key] : '';
      let visible = false;
      const override = input(`${group}_${key}_override`, '', { type: 'checkbox', checked: present, 'aria-label': `Override ${key} at this scope` });
      const control = valueControl(`${group}_${key}`, present ? value : undefined, shape, sensitive);
      control.disabled = !present;
      fields.set(`${group}.${key}`, control);
      const evidence = effective?.[group]?.[key];
      const help = [shape.description, shape.pass ? `Asset ${shape.pass}` : '', shape.excluded_by?.length ? `Excluded by: ${shape.excluded_by.map(item => item.runtime).join(', ')}` : ''].filter(Boolean).join(' · ');
      const provenance = evidence ? [evidence.tier, evidence.node, evidence.model, evidence.pattern, evidence.endpoint].filter(Boolean).join(' · ') : '';
      const row = el('section', { class: 'parameter-setting' }, el('div', { class: 'detail-heading' }, el('h4', {}, key), text(shape.type || 'string', 'muted')),
        help ? text(help, 'muted') : '', el('label', { class: 'model-choice' }, override, 'Override at this scope'), field('Value', control),
        evidence ? text(sensitive ? `Effective value hidden · ${provenance}` : `Effective: ${String(evidence.value)} · ${provenance}`, 'muted') : text('No effective value reported.', 'muted'));
      const updateValue = () => { if (override.checked) update(group, key, value); };
      control.addEventListener('input', () => { value = control.value; updateValue(); });
      control.addEventListener('change', () => { value = control.value; updateValue(); });
      override.addEventListener('change', () => {
        control.disabled = !override.checked;
        if (override.checked) updateValue(); else update(group, key, null);
      });
      if (sensitive) {
        const reveal = button('Reveal', () => {
          if (controls.disabled) return;
          visible = !visible;
          control.type = visible ? 'text' : 'password';
          control.value = visible ? value : '';
          reveal.textContent = visible ? 'Hide' : 'Reveal';
        });
        row.append(actions(reveal), text('Values are hidden until revealed. Entering a value replaces this scope only.', 'muted'));
        owner.scope.signal.addEventListener('abort', () => { value = ''; control.value = ''; }, { once: true });
      }
      entries.push({ root: row, search: `${key} ${help}`.toLowerCase() });
      (sensitive ? environmentRows : parameterRows).append(row);
    };
    for (const group of ['parameters', 'environment']) {
      const keys = [...new Set([...Object.keys(shapes[group]), ...Object.keys(before[group] || {}), ...Object.keys(effective?.[group] || {})])].sort();
      for (const key of keys) addRow(group, key);
    }
    const envName = input('environment_name', '', { placeholder: 'e.g. CUDA_VISIBLE_DEVICES', 'aria-label': 'Environment variable name' });
    const addEnv = button('Add variable', () => {
      const key = envName.value.trim();
      if (controls.disabled || !key) return;
      if (fields.has(`environment.${key}`)) { fields.get(`environment.${key}`).focus(); return; }
      addRow('environment', key); envName.value = ''; filter();
    });
    controls.append(parameterRows.childNodes.length ? parameterRows : message('No runtime parameters declared for this provider.'),
      el('details', { class: 'access-section' }, el('summary', {}, 'Environment variables'), environmentRows, actions(envName, addEnv)));
    const save = el('button', { type: 'submit', class: 'primary' }, 'Save settings');
    form.addEventListener('submit', async event => {
      event.preventDefault();
      if (!form.reportValidity() || blocked || controls.disabled || !owner.api.canChange()) return;
      if (!Object.keys(changes.parameters).length && !Object.keys(changes.environment).length) { feedback.replaceChildren(message('No settings changed.')); return; }
      controls.disabled = true; save.disabled = true;
      feedback.replaceChildren(message('Checking current settings…'));
      try {
        const fresh = await owner.api.request(`${base(provider)}/parameters?${query}`, { signal: owner.scope.signal });
        if (!owner.current()) return;
        const conflicts = parameterConflicts(before, fresh, changes);
        if (conflicts.length) {
          blocked = true;
          throw new Error(`These settings changed in another session: ${conflicts.join(', ')}. Reload before editing.`);
        }
        const result = await owner.api.request(`${base(provider)}/parameters`, { method: 'PATCH', contentType: 'application/merge-patch+json', body: parameterPatch(selection, changes, shapes), signal: owner.scope.signal });
        if (!owner.current()) return;
        blocked = true; draft.clean();
        await readAfterWrite(owner, notice => load(selection, notice, result.runs), 'Settings saved. Changes take effect on the next start; per-run overrides can still take precedence.');
      } catch (error) {
        if (!owner.current()) return;
        blocked ||= !!error.uncertain || !!error.mutation_applied;
        validation.error(error);
        if (blocked) feedback.append(leaveButton('Inspect current settings', () => { draft.clean(); return load(selection); }, owner.api));
      } finally {
        if (owner.current()) { controls.disabled = blocked; save.disabled = blocked; owner.syncActivity?.(); }
      }
    });
    form.append(feedback, controls, text('Only overrides changed here are saved. Clearing an override restores inheritance. Values are checked again before saving, but another session can still change them during the save.', 'muted'),
      editorActions(save, () => { if (draft.discard()) { draft.clean(); back(); } }, owner.api), count);
    owner.root.replaceChildren(el('h2', {}, `${provider} settings`), notice ? message(notice) : '', runs ? runImpact(owner, runs) : '', scopeForm,
      text(`Effective values below describe ${selection.node || 'the coordinator'}${selection.model ? ` · ${selection.model}` : ''}. They can differ from this editing scope.`, 'muted'),
      !effective ? message('Effective values could not be read. Configured overrides remain available.', 'warning') : '', search, form);
    owner.scope.signal.addEventListener('abort', () => {
      before.environment = {}; if (effective) effective.environment = {};
      changes.environment = Object.create(null); clearSecrets(owner.root);
    }, { once: true });
    changed(); owner.syncActivity?.();
  };
  await load();
}

function runImpact(p, runs) {
  return el('details', { class: 'access-section' }, el('summary', {}, 'Effect on running models'),
    runs.error ? message(runs.error, 'warning') : '',
    ...[['stale', 'Need restart'], ['overridden', 'Have per-run overrides'], ['unknown', 'Effect unknown']].map(([key, label]) =>
      el('section', {}, el('h4', {}, `${label}: ${runs[key]?.length || 0}`), el('ul', {}, (runs[key] || []).map(run => el('li', {}, `${run.node} · ${run.model || run.id}${run.reason ? `: ${run.reason}` : ''}`))))),
    leaveButton('View workloads', () => p.go('workloads'), p.api));
}
