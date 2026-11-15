import { Lifetime } from './api.mjs';
import { el, text, button, field, select, message, editorActions, formFeedback } from './dom.mjs';
import { launchTargets, runAssessment } from './workload-data.mjs';
import { followStart } from './workload-details.mjs';

export async function startRun(p, nodes, initialNode, initialTarget) {
  p.editing = true;
  const form = el('form', { class: 'entity-editor' });
  const draft = p.trackDraft?.(form), choices = new Lifetime();
  const controls = el('fieldset', { 'data-protected-actions': true });
  const node = select('run_node', nodes.map(value => [value.name, value.name]), initialNode || nodes[0]?.name);
  const target = select('run_target', [['', 'Loading targets…']], ''); target.required = true;
  const endpoint = select('run_endpoint', [['chat', 'Chat'], ['embeddings', 'Embeddings'], ['reranking', 'Reranking']], 'chat');
  const feedback = el('div', { 'aria-live': 'polite' });
  const validation = formFeedback(form, feedback, key => ({ node, provider: target, model: target, endpoint })[key]);
  const preview = button('Preview start', previewStart);
  const save = el('button', { type: 'submit', class: 'primary', disabled: true }, 'Start run');
  let targets = [], reviewed, blocked = false, busy = false;
  const body = () => {
    const value = targets[Number(target.value)];
    if (!value || target.value === '') return null;
    return { node: value.node, provider: value.provider, ...(value.model ? { model: value.model, endpoint: endpoint.value } : {}) };
  };
  const invalidate = () => { reviewed = undefined; save.disabled = true; feedback.replaceChildren(); endpoint.disabled = !body()?.model; };
  controls.append(el('h2', {}, 'Start a model or service'), text('Choose a local model or service, then review its capacity and start settings.', 'muted'),
    field('Compute node', node), field('Model or service', target), field('Endpoint', endpoint), preview);
  form.append(feedback, controls, editorActions(save, p.close, p.api));
  p.root.append(form);
  p.scope.signal.addEventListener('abort', () => choices.end(), { once: true });
  node.addEventListener('change', () => { if (!busy && !blocked) void loadTargets(); });
  target.addEventListener('change', invalidate); endpoint.addEventListener('change', invalidate);

  async function loadTargets() {
    invalidate(); targets = []; target.disabled = true; preview.disabled = true;
    target.replaceChildren(el('option', { value: '' }, 'Loading targets…'));
    const scope = choices.next();
    const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    const selectedNode = node.value;
    try {
      const [models, providers] = await Promise.all([
        p.api.list(`/models?node=${encodeURIComponent(selectedNode)}`, { signal, timeoutMs: 45000 }),
        p.api.list(`/providers?node=${encodeURIComponent(selectedNode)}`, { signal }),
      ]);
      if (!p.current() || !scope.current()) return;
      targets = launchTargets(models, providers, selectedNode);
      target.replaceChildren(el('option', { value: '' }, 'Choose a model or service…'), ...targets.map((item, index) => el('option', { value: String(index) }, item.label)));
      const chosen = initialTarget && selectedNode === initialNode ? targets.findIndex(item => item.model === initialTarget.model && item.provider === initialTarget.provider) : -1;
      target.value = chosen >= 0 ? String(chosen) : '';
      endpoint.disabled = !body()?.model;
      target.disabled = !targets.length; preview.disabled = !targets.length;
      if (!targets.length) feedback.replaceChildren(message('No complete local model or node service with a reported run lifecycle is available here. Check the node’s provider setup and model inventory.', 'warning'));
    } catch (error) {
      if (p.current() && scope.current()) { validation.error(error); feedback.append(button('Reload targets', loadTargets)); }
    }
  }
  async function previewStart() {
    const selected = body();
    if (!form.reportValidity() || !selected || busy || blocked || !p.current() || !p.api.canChange()) return;
    busy = true; controls.disabled = true; save.disabled = true;
    feedback.replaceChildren(message('Preparing the start preview. This may take up to two minutes…'));
    try {
      const result = await p.api.data('/runs/preview', { method: 'POST', effectful: false, body: selected, signal: p.scope.signal, timeoutMs: 120000 });
      if (!p.current()) return;
      reviewed = JSON.stringify(selected);
      feedback.replaceChildren(runAssessment(result)); save.disabled = false;
    } catch (error) { if (p.current()) { reviewed = undefined; validation.error(error); } }
    finally { busy = false; if (p.current()) controls.disabled = blocked; }
  }
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const selected = body();
    if (!selected || busy || blocked || !p.current() || !p.api.canChange() || reviewed !== JSON.stringify(selected)) return;
    busy = true; blocked = true; controls.disabled = true; save.disabled = true;
    const operation = crypto.randomUUID();
    const submitted = Object.freeze({ ...selected, auto_deploy: false });
    feedback.replaceChildren(message('Starting the selected target. Waiting for the node…'));
    let result, error;
    try { result = await p.api.data('/runs', { method: 'POST', body: submitted, idempotencyKey: operation, signal: p.scope.signal, timeoutMs: 120000 }); }
    catch (reason) { error = reason; }
    if (!p.current()) return;
    draft?.clean();
    const owner = p.panel ? p.panel.view('Start result') : p;
    await followStart(owner, submitted, operation, result, error);
  });
  if (nodes.length) await loadTargets();
  else feedback.replaceChildren(message('No compute nodes are available. Refresh the node inventory before starting a run.', 'warning'));
}
