import { Lifetime } from './api.mjs';
import { el, text, button, field, input, select, message, editorActions, formFeedback } from './dom.mjs';
import { launchTargets, runAssessment } from './workload-data.mjs';
import { nodeHealth } from './network-data.mjs';
import { followStart } from './workload-details.mjs';
import { parameterFields, parameterShapes, providerSchemaPath, typedParameterValue } from './parameters.mjs';

export async function startRun(p, nodes, initialNode, initialTarget, { run, onReady, endpointKinds, back = p.close } = {}) {
  p.editing = true;
  const form = el('form', { class: 'entity-editor' });
  const draft = p.trackDraft?.(form), choices = new Lifetime();
  const controls = el('fieldset', { 'data-protected-actions': true });
  const canStart = host => host.health_status === 'healthy' && host.providers?.some(item =>
    ['process', 'served'].includes(item.run_lifecycle) && (item.run_lifecycle !== 'served' || item.running !== false));
  const ready = nodes.filter(canStart);
  const node = select('run_node', [['', 'Choose a compute node…']], '');
  node.disabled = !!run;
  node.required = true;
  for (const host of nodes) {
    const available = canStart(host);
    const reason = host.health_status === 'healthy' ? 'No runnable local provider' : nodeHealth(host)[0];
    node.append(el('option', { value: host.name, disabled: !available }, available ? host.name : `${host.name} (${reason})`));
  }
  node.value = (initialNode ? ready.find(host => host.name === initialNode) : ready[0])?.name || '';
  const target = select('run_target', [['', 'Loading targets…']], ''); target.required = true;
  const endpoints = [['chat', 'Chat'], ['embeddings', 'Embeddings'], ['reranking', 'Reranking']].filter(([id]) => !endpointKinds || endpointKinds.includes(id));
  const endpoint = select('run_endpoint', endpoints, run?.endpoint || endpoints[0]?.[0]);
  const replace = input('run_replace', '', { type: 'checkbox' });
  const settings = el('details', { class: 'access-section', open: !!run }, el('summary', {}, 'Settings for this run'));
  const settingsBody = el('div'); settings.append(settingsBody);
  const feedback = el('div', { 'aria-live': 'polite' });
  const validation = formFeedback(form, feedback, key => ({ node, provider: target, model: target, endpoint, replace })[key] || editor?.target(key));
  const preview = button('Preview start', previewStart);
  const save = el('button', { type: 'submit', class: 'primary', disabled: true }, 'Start run');
  let targets = [], reviewed, blocked = false, busy = false, editor, settingsReady = false, settingsLoading = false;
  let values;
  const settingsReads = new Lifetime();
  const body = () => {
    const value = targets[Number(target.value)];
    if (!value || target.value === '') return null;
    const { parameters, environment } = values || {};
    return { node: value.node, provider: value.provider, ...(value.model ? { model: value.model, endpoint: endpoint.value } : {}),
      ...(parameters && Object.keys(parameters).length ? { parameters: { ...parameters } } : {}),
      ...(environment && Object.keys(environment).length ? { environment: { ...environment } } : {}) };
  };
  const invalidate = () => { reviewed = undefined; save.disabled = true; feedback.replaceChildren(); endpoint.disabled = !!run || !body()?.model; };
  controls.append(el('h2', {}, 'Start a model or service'), text('Choose a local model or service, then review its capacity and start settings.', 'muted'),
    field('Compute node', node), field('Model or service', target), field('Endpoint', endpoint), settings,
    el('label', { class: 'model-choice' }, replace, 'Replace the running run'), preview);
  form.append(feedback, controls, editorActions(save, back, p.api, draft));
  p.root.append(form);
  p.scope.signal.addEventListener('abort', () => { choices.end(); settingsReads.end(); values = undefined; }, { once: true });
  node.addEventListener('change', () => { if (!busy && !blocked) void loadTargets(); });
  target.addEventListener('change', resetSettings); endpoint.addEventListener('change', resetSettings);
  replace.addEventListener('change', invalidate);
  settings.addEventListener('toggle', () => { if (settings.open && !settingsReady && !settingsLoading && !busy) void resetSettings(); });

  async function resetSettings() {
    const scope = settingsReads.next();
    settingsBody.replaceChildren(); values = undefined; editor = undefined; settingsReady = settingsLoading = false; invalidate();
    const selected = body();
    if (!settings.open || !selected) return;
    settingsLoading = preview.disabled = true;
    settingsBody.append(message('Loading settings for this run…'));
    try {
      const schema = await p.api.request('/zzrouter/v1' + providerSchemaPath(selected.provider), { signal: AbortSignal.any([p.scope.signal, scope.signal]) });
      if (!p.current() || !scope.current()) return;
      const shapes = parameterShapes(schema, selected.endpoint);
      values = {
        parameters: Object.assign(Object.create(null), Object.fromEntries(Object.entries(run?.parameters || {}).map(([key, value]) => [key, typedParameterValue(String(value), shapes.parameters[key])]))),
        environment: Object.assign(Object.create(null), run?.environment),
      };
      const editorOwner = { ...p, scope: { signal: AbortSignal.any([p.scope.signal, scope.signal]) } };
      editor = parameterFields(editorOwner, { shapes, values, disabled: () => controls.disabled, update: (group, key, value) => {
        if (value === null) delete values[group][key];
        else values[group][key] = typedParameterValue(value, shapes[group][key]);
        invalidate();
      } });
      settingsBody.replaceChildren(editor.root); settingsReady = true;
    } catch (error) {
      if (p.current() && scope.current()) { validation.error(error); settingsBody.replaceChildren(button('Reload run settings', resetSettings)); }
    } finally {
      if (p.current() && scope.current()) { settingsLoading = false; preview.disabled = !!run && !settingsReady; }
    }
  }

  async function loadTargets() {
    targets = []; void resetSettings(); target.disabled = true; preview.disabled = true;
    const scope = choices.next();
    const selectedNode = node.value;
    if (!ready.some(host => host.name === selectedNode)) {
      target.replaceChildren(el('option', { value: '' }, 'Choose a compute node first'));
      if (!ready.length || initialNode) feedback.replaceChildren(message(
        ready.length ? `${initialNode} has no available local runtime. Choose another node.` :
          'No healthy node reports a runnable local provider.', 'warning'));
      return;
    }
    target.replaceChildren(el('option', { value: '' }, 'Loading targets…'));
    const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    try {
      const [models, providers] = await Promise.all([
        p.api.list(`/models?node=${encodeURIComponent(selectedNode)}`, { signal, timeoutMs: 45000 }),
        p.api.list(`/providers?node=${encodeURIComponent(selectedNode)}`, { signal }),
      ]);
      if (!p.current() || !scope.current()) return;
      targets = launchTargets(models, providers, selectedNode);
      target.replaceChildren(el('option', { value: '' }, 'Choose a model or service…'), ...targets.map((item, index) => el('option', { value: String(index) }, item.label)));
      const matches = initialTarget && selectedNode === initialNode ? targets.filter(item => item.model === initialTarget.model && (!initialTarget.provider || item.provider === initialTarget.provider)) : [];
      target.value = matches.length === 1 ? String(targets.indexOf(matches[0])) : '';
      await resetSettings();
      if (!p.current() || !scope.current()) return;
      target.disabled = !!run || !targets.length; preview.disabled = !targets.length || !!run && !settingsReady;
      if (run && matches.length !== 1) feedback.replaceChildren(message('This exact run target is unavailable in the local launch inventory.', 'warning'));
      if (!targets.length) feedback.replaceChildren(message('No complete local model or node service with a reported run lifecycle is available here. Check the node’s provider setup and model inventory.', 'warning'));
    } catch (error) {
      if (p.current() && scope.current()) { validation.error(error); feedback.append(button('Reload targets', loadTargets)); }
    }
  }
  async function previewStart() {
    const selected = body();
    if (!form.reportValidity() || !selected || settings.open && !settingsReady || busy || blocked || !p.current() || !p.api.canChange()) return;
    busy = true; controls.disabled = true; save.disabled = true;
    feedback.replaceChildren(message('Preparing the start preview. This may take up to two minutes…'));
    try {
      const result = await p.api.data('/runs/preview', { method: 'POST', effectful: false, body: selected, signal: p.scope.signal, timeoutMs: 120000 });
      if (!p.current()) return;
      reviewed = JSON.stringify([selected, replace.checked]);
      feedback.replaceChildren(runAssessment(result)); save.disabled = false;
      if (replace.checked) feedback.append(message('Replacing the running run can interrupt active work.', 'warning'));
      if (run?.pinned && !Object.keys(selected.parameters || {}).length && !Object.keys(selected.environment || {}).length) {
        feedback.append(message('The node reuses retained pin settings when both request maps are empty.', 'warning'));
      }
    } catch (error) { if (p.current()) { reviewed = undefined; validation.error(error); } }
    finally { busy = false; if (p.current()) controls.disabled = blocked; }
  }
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const selected = body();
    if (!form.reportValidity() || !selected || busy || blocked || !p.current() || !p.api.canChange() || reviewed !== JSON.stringify([selected, replace.checked])) return;
    busy = true; blocked = true; controls.disabled = true; save.disabled = true;
    const operation = crypto.randomUUID();
    const submitted = Object.freeze({ ...selected, auto_deploy: false, ...(replace.checked ? { replace: true } : {}) });
    feedback.replaceChildren(message('Starting the selected target. Waiting for the node…'));
    let result, error;
    try { result = await p.api.data('/runs', { method: 'POST', body: submitted, idempotencyKey: operation, signal: p.scope.signal, timeoutMs: 120000 }); }
    catch (reason) { error = reason; }
    if (!p.current()) return;
    if (error?.code === 'run_settings_differ' && !replace.checked) {
      busy = blocked = controls.disabled = false;
      reviewed = undefined;
      validation.error(error);
      feedback.append(message('The running process uses a different request tier. Replacing it can interrupt active work.', 'warning'),
        button('Choose replacement', () => { replace.checked = true; draft?.touch(); invalidate(); preview.focus(); }));
      return;
    }
    draft?.clean();
    const owner = p.panel ? p.panel.view('Start result') : p;
    await followStart(owner, submitted, operation, result, error, onReady);
  });
  await loadTargets();
}
