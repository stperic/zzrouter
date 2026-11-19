import { formSubmission, readAfterWrite } from './details.mjs';
import { changedFields } from './api.mjs';
import { el, text, field, input, select, button, leaveButton, actions, message, editorActions, formFeedback } from './dom.mjs';
import { providerCategory } from './inventory.mjs';

// Keep every editable replica field, excluding live observations from GET.
export function routeValues(record = {}) {
  return {
    description: record.description || '', strategy: record.strategy || 'priority',
    replicas: (record.replicas || []).map(({ cooldown_seconds, cooldown_reason, ...replica }) => ({ node: '', on_demand: false, ...structuredClone(replica) })),
  };
}

function providerOptions(candidates, selected, placeholder, saved) {
  const option = (value, label) => el('option', { value, selected: value === selected }, label);
  const groups = [['local', 'Local'], ['cloud', 'Cloud']].flatMap(([kind, label]) => {
    const items = [...candidates.values()].filter(item => providerCategory(item) === kind);
    return items.length ? [el('optgroup', { label: `${label} providers` },
      items.map(item => option(item.key, `${item.name || item.key} (${label})`)))] : [];
  });
  return [option('', placeholder), ...groups, ...(saved ? [option(...saved)] : [])];
}

export function routeForm(p, record, { catalog, models, nodes, etag, teams, incomplete, invalidatePreview, cancel = p.close, saved }, reload) {
  const before = routeValues(record || {});
  const form = el('form', { class: 'entity-editor route-editor' });
  const draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const feedback = el('div', { 'aria-live': 'polite' });
  const name = input('route_name', record?.name || '', { required: true, readOnly: !!record, placeholder: 'e.g. coding' });
  const description = input('route_description', before.description, { placeholder: 'What should clients use this route for?' });
  const strategy = select('route_strategy', [['priority', 'Priority order'], ['least-load', 'Least loaded'], ['fastest', 'Fastest observed']], before.strategy);
  controls.append(field('Route name', name, 'Clients send this exact name in the model field.'), field('Description', description),
    field('Selection strategy', strategy, 'Priority tries destinations in order. Other strategies choose among all eligible destinations, including cloud providers.'));
  if (record?.auto_managed) controls.append(message('Saving takes ownership of this automatic route. Its destinations will then be managed here.', 'warning'));
  if (incomplete) controls.append(message('Some inventory choices could not be loaded. Saved destinations are retained. Reload for the complete inventory.', 'warning'));
  const destinations = el('div', { class: 'route-destinations' });
  let rows = before.replicas.map((replica, index) => ({ replica, expanded: index === 0 }));
  if (!record) rows.push({ replica: { name: 'destination-1', provider: '', model: '', node: '', priority: 1 }, expanded: true });
  const validation = formFeedback(form, feedback, key => {
    const field = { name, description, strategy }[key];
    const replica = /^replicas\[(\d+)\]\.(node|provider|model|on_demand)$/.exec(key || '');
    return field || (replica && rows[Number(replica[1])]?.fields[replica[2]]);
  });
  const capture = () => { for (const row of rows) if (row.read) { row.replica = row.read(); row.expanded = row.panel.open; } };
  const render = () => {
    validation.clear(); feedback.replaceChildren();
    destinations.replaceChildren();
    rows.forEach((row, index) => {
      const rep = row.replica;
      const nodeNames = [...new Set([...nodes.map(node => node.name), rep.node].filter(Boolean))];
      const node = select(`node_${index}`, [['', `Coordinator (${p.state.node_name || 'local'})`], ...nodeNames.map(value => [value, value])], rep.node || '');
      const provider = select(`provider_${index}`, [], rep.provider);
      provider.required = true;
      const model = input(`model_${index}`, rep.model, { required: true, list: `route-models-${index}`, placeholder: 'Choose or enter an exact model' });
      const choices = el('datalist', { id: `route-models-${index}` });
      const demand = input(`on_demand_${index}`, '', { type: 'checkbox', checked: !!rep.on_demand });
      const startup = el('label', { class: 'model-choice' }, demand, 'Start model on demand');
      const syncProviderFields = () => {
        const targetNode = node.value || p.state.node_name;
        const observed = nodes.find(item => item.name === targetNode)?.providers?.find(item => item.key === provider.value);
        const declared = catalog.find(item => item.key === provider.value);
        const selected = declared || observed;
        const lifecycle = observed?.run_lifecycle;
        const canStart = selected && providerCategory(selected) === 'local' && observed?.run_scope !== 'node' &&
          (lifecycle ? ['process', 'served'].includes(lifecycle) :
            selected.kind === 'on-demand' || selected.mode === 'on-demand' || selected.protocol === 'ollama');
        startup.hidden = demand.disabled = !canStart;
        choices.replaceChildren(...[...new Set(models.filter(item =>
          (!item.assigned_app || item.assigned_app === provider.value) &&
          (!item.node || item.node === targetNode)
        ).map(item => item.name).filter(Boolean))].sort().map(value => el('option', { value })));
      };
      const populateProviders = (saved = '') => {
        const candidates = new Map(catalog.filter(item => ['openai', 'ollama'].includes(item.protocol)).map(item => [item.key, item]));
        const targetNode = node.value || p.state.node_name;
        // Workers can report provider instances absent from the coordinator catalog.
        // A cloud service or assigned model is positive inference evidence; a node
        // observation alone must not turn a registry or browser tool into a route.
        for (const item of nodes.find(item => item.name === targetNode)?.providers || []) {
          if (catalog.some(entry => entry.key === item.key)) continue;
          if (providerCategory(item) === 'cloud' || models.some(model => model.node === targetNode && model.assigned_app === item.key)) candidates.set(item.key, item);
        }
        const unavailable = saved && !candidates.has(saved) ? [saved, `${saved} (saved)`] : null;
        provider.replaceChildren(...providerOptions(candidates, saved, 'Choose a provider…', unavailable));
        syncProviderFields();
      };
      node.addEventListener('change', () => populateProviders(provider.value));
      provider.addEventListener('change', syncProviderFields);
      populateProviders(rep.provider);
      row.read = () => ({
        ...rep, provider: provider.value, node: node.value, model: model.value.trim(),
        on_demand: demand.disabled ? provider.value === rep.provider && !!rep.on_demand : demand.checked,
      });
      row.fields = { node, provider, model, on_demand: demand };
      const move = direction => {
        if (controls.disabled || !p.api.canLeave()) return;
        draft?.touch(); capture(); const target = index + direction;
        [rows[index], rows[target]] = [rows[target], rows[index]];
        rows.forEach((item, position) => { item.replica.priority = position + 1; });
        rows[target].expanded = true;
        render(); destinations.children[target]?.querySelector?.('select')?.focus();
      };
      const up = button('↑', () => move(-1)); up.setAttribute('aria-label', `Move destination ${index + 1} up`); up.disabled = index === 0;
      const down = button('↓', () => move(1)); down.setAttribute('aria-label', `Move destination ${index + 1} down`); down.disabled = index === rows.length - 1;
      const remove = button('Remove', () => { if (!controls.disabled && p.api.canLeave()) { draft?.touch(); capture(); rows.splice(index, 1); render(); } }, 'link');
      remove.disabled = rows.length === 1;
      const summary = el('summary', {});
      const describe = () => summary.replaceChildren(el('strong', {}, `${index + 1}. ${provider.value || 'Choose a provider'}`),
        el('small', {}, [model.value || 'Choose a model', node.value || p.state.node_name || 'Coordinator'].join(' · ')));
      for (const control of [node, provider, model]) control.addEventListener('input', describe);
      describe();
      row.panel = el('details', { class: 'route-destination', open: row.expanded }, summary,
        el('div', { class: 'route-destination-heading' }, text(`Destination ${index + 1}`, 'muted'), actions(up, down, remove)),
        field('Compute node', node), field('Provider', provider), field('Model', model), choices, startup);
      destinations.append(row.panel);
    });
  };
  const add = button('+ Add destination', () => {
    if (controls.disabled || !p.api.canLeave()) return;
    draft?.touch(); capture(); let next = rows.length + 1;
    while (rows.some(row => row.replica.name === `destination-${next}`)) next++;
    rows.push({ replica: { name: `destination-${next}`, provider: '', model: '', node: '', priority: Math.max(0, ...rows.map(row => row.replica.priority || 0)) + 1 }, expanded: true });
    render();
    rows.at(-1).fields.node.focus();
  });
  controls.append(el('section', { class: 'access-section' }, el('h3', {}, 'Destinations'), text('Cloud destinations can incur charges. Team permissions and spending limits always apply.', 'muted'), destinations, add));
  render();
  if (teams.length) controls.append(message(`Changes affect ${teams.length} assigned ${teams.length === 1 ? 'team' : 'teams'}: ${teams.map(team => team.name).join(', ')}.`));
  const save = el('button', { type: 'submit', class: 'primary' }, record ? 'Save changes' : 'Create route');
  const submission = formSubmission(p, controls, save, draft);
  const write = async (method, body) => {
    const path = `/model-groups/${encodeURIComponent(record?.name || name.value.trim())}`;
    await submission.run(() => {
      invalidatePreview?.();
      feedback.replaceChildren(message('Saving route…'));
      return p.api.data(path, { method, body, signal: p.scope.signal, ifMatch: record ? etag : undefined, ifNoneMatch: record ? undefined : '*' });
    }, () => readAfterWrite(p, () => saved ? saved(record?.name || name.value.trim(), method === 'DELETE') : reload(), method === 'DELETE' ? 'Route deleted.' : 'Route saved.'), (error, blocked) => {
      if (error.status === 412) submission.block();
      validation.error(error);
      if (blocked || submission.blocked) feedback.append(leaveButton('Reload routes', reload, p.api));
    });
  };
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!form.reportValidity() || controls.disabled || !p.api.canChange()) return;
    capture();
    const after = { description: description.value.trim(), strategy: strategy.value, replicas: rows.map(row => row.replica) };
    const body = record ? changedFields(before, after) : after;
    if (!Object.keys(body).length) { feedback.replaceChildren(message('No fields changed.')); return; }
    await write(record ? 'PATCH' : 'PUT', body);
  });
  if (record) {
    const remove = button('Delete route', () => { if (confirm(`Delete route ${record.name}? Clients using this name will stop working.`)) void write('DELETE'); }, 'danger');
    remove.disabled = !!record.auto_managed || teams.length > 0;
    controls.append(el('section', { class: 'access-section' }, el('h3', {}, 'Lifecycle'), remove,
      text(teams.length ? 'Remove team assignments before deleting this route.' : record.auto_managed ? 'Save to take ownership before deleting an automatic route.' : 'Deletion cannot be undone.', 'muted')));
  }
  if (record && !etag) { submission.block(); feedback.append(message('The route version could not be read. Reload before editing.', 'warning')); }
  const footer = editorActions(save, cancel, p.api, draft);
  form.append(feedback, controls, footer);
  return form;
}
