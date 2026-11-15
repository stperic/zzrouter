import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, actions, field, input, select, facts, message, problem, editorActions, formFeedback, copyField } from './dom.mjs';
import { providerName } from './provider-icons.mjs';
import { providerPath, providerOperation } from './provider-actions.mjs';

export function acceptedInstallOptions(plan, options = {}) {
  const body = { expected_plan_id: plan.plan_id, version: plan.version, runtime: plan.provider, action: plan.action };
  for (const key of ['force', 'disposable', 'smoke']) if (options[key]) body[key] = options[key];
  if (Array.isArray(plan.resolved_install?.selected_components)) body.components = [...plan.resolved_install.selected_components];
  return body;
}
function installContext(step) {
  return [['Working directory', step.working_dir], ['Run as', step.run_as], ['Environment', Object.keys(step.env || {}).length ? 'Supplied by the managed installer' : ''],
    ['Standard input', step.stdin ? 'Supplied by the managed installer' : ''], ['Service action', step.service_action], ['Timeout', step.timeout], ['Optional step', step.optional ? 'Failure is handled by the installer' : ''],
    ['Runtime verification', step.verify?.type === 'runtime_checks' ? 'Performed by the managed installer' : '']].filter(([, value]) => value);
}
export function verificationResult(value) {
  const status = value.unverifiable ? 'Not verifiable' : value.passed ? 'Passed' : 'Failed';
  return el('section', { class: 'access-section' }, text(`Step ${value.step}: ${status}`), value.message ? text(value.message) : null, value.actual ? text(value.actual, 'muted') : null,
    ...(value.checks || []).map(check => facts(Object.entries(check).filter(([, item]) => typeof item === 'string' || typeof item === 'boolean' || typeof item === 'number'))));
}

export async function installProvider(p, entry, node, back, initial = {}) {
  const form = el('form', { class: 'entity-editor' }), draft = p.trackDraft?.(form), controls = el('fieldset', { 'data-protected-actions': true });
  const runtime = select('install_runtime', [['', 'Provider default'], ...(initial.runtime ? [[initial.runtime, initial.runtime]] : [])], initial.runtime || '');
  const version = input('install_version', initial.version || '');
  const force = input('install_force', '', { type: 'checkbox', checked: Boolean(initial.force) });
  const components = el('div'), feedback = el('div', { 'aria-live': 'polite' }), preflight = el('div');
  const preview = el('button', { type: 'submit', class: 'primary' }, 'Review install plan');
  const check = button('Check prerequisites', checkPrerequisites);
  const validation = formFeedback(form, feedback, key => ({ version, runtime, components, force })[key]);
  const environmentReads = new Lifetime(); p.scope.signal.addEventListener('abort', () => environmentReads.end(), { once: true });
  let componentInputs = new Map(), busy = false, componentsChanged = Boolean(initial.components), componentReady = false;
  controls.append(providerName(entry.key, entry.name, 'h2'), text(`Compute node: ${node}`, 'muted'), field('Runtime', runtime), field('Version (optional)', version, 'Leave blank to let the coordinator resolve the configured release.'), components,
    el('details', {}, el('summary', {}, 'Replacement options'), field('Force replacement of the managed installation', force, 'Review the resulting plan before execution.')),
    check, preflight);
  form.append(feedback, controls, editorActions(preview, back, p.api, draft)); p.root.append(form);
  async function readComponents() {
    const scope = environmentReads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    componentReady = false; preview.disabled = true; components.replaceChildren(message('Reading runtime components…')); componentInputs = new Map();
    try {
      const suffix = '/environment';
      const result = await p.api.data(providerPath(entry.key, node, suffix) + (runtime.value ? '&runtime=' + encodeURIComponent(runtime.value) : ''), { signal, timeoutMs: 60000 });
      if (!p.current() || !scope.current()) return;
      const declared = result.desired_install?.recipe?.components || {};
      const selected = runtime.value === (initial.runtime || '') && initial.components ? initial.components : result.installed_manifest?.selected_components || [];
      components.replaceChildren(...(Object.keys(declared).length ? [el('h3', {}, 'Optional components'), ...Object.entries(declared).map(([name, spec]) => {
        const control = input('component_' + name, '', { type: 'checkbox', checked: selected.includes(name) }); componentInputs.set(name, control);
        control.addEventListener('change', () => { componentsChanged = true; });
        return field(name, control, spec.description || spec.requirements || 'Release-declared component');
      })] : [text('This runtime declares no optional components.', 'muted')]));
      componentReady = true; preview.disabled = false;
    } catch (error) {
      if (!p.current() || !scope.current()) return;
      components.replaceChildren(message('Optional components could not be observed. Omitted component selection preserves the server-managed selection.', 'warning'), problem(error), leaveButton('Retry component read', readComponents, p.api));
      // Legacy binary installers have no environment contract. Preview remains
      // authoritative and can disclose components even without this observation.
      componentsChanged = false; componentReady = true; preview.disabled = false;
    }
  }
  function options() {
    return { ...(version.value.trim() ? { version: version.value.trim() } : {}), ...(runtime.value ? { runtime: runtime.value } : {}), ...(force.checked ? { force: true } : {}),
      ...(componentsChanged ? { components: [...componentInputs].filter(([, control]) => control.checked).map(([name]) => name) } : {}) };
  }
  async function checkPrerequisites() {
    if (busy || !p.current() || !p.api.canChange()) return;
    busy = true; controls.disabled = true; preview.disabled = true;
    preflight.replaceChildren(message('Checking prerequisites on the selected node…'));
    try {
      const report = await p.api.data(providerPath(entry.key, node, '/install/preflight'), { method: 'POST', effectful: false, signal: p.scope.signal, timeoutMs: 60000 });
      if (!p.current()) return;
      preflight.replaceChildren(message(report.all_ok ? 'All reported prerequisites passed.' : 'Some prerequisites need attention.', report.all_ok ? '' : 'warning'), ...(report.results || []).map(item => el('div', {}, facts([['Check', item.check], ['Result', item.passed ? 'Passed' : 'Failed']]), text(item.message), item.hint ? text(item.hint, 'muted') : null)));
    } catch (error) { if (p.current()) preflight.replaceChildren(problem(error)); }
    finally { busy = false; if (p.current()) { controls.disabled = false; preview.disabled = !componentReady; } }
  }
  form.addEventListener('submit', async event => {
    event.preventDefault(); if (busy || !componentReady || !p.current() || !p.api.canChange() || !form.reportValidity()) return;
    busy = true; controls.disabled = true; preview.disabled = true;
    const submitted = options(); feedback.replaceChildren(message('Preparing the exact install plan…'));
    try {
      const plan = await p.api.data(providerPath(entry.key, node, '/install/plan'), { method: 'POST', effectful: false, body: submitted, signal: p.scope.signal, timeoutMs: 120000 });
      if (!p.current()) return;
      draft?.clean();
      await installReview(p.panel.view('Review install plan'), entry, node, plan, submitted, back);
    } catch (error) { if (p.current()) validation.error(error); }
    finally { busy = false; if (p.current()) { controls.disabled = false; preview.disabled = !componentReady; } }
  });
  runtime.addEventListener('change', () => { componentsChanged = false; void readComponents(); });
  preview.disabled = true;
  try {
    const schema = await p.api.request('/zzrouter/v1' + providerPath(entry.key, node, '/schema'), { signal: p.scope.signal });
    if (!p.current()) return;
    const names = Object.keys(schema.diagnostics?.runtimes || {});
    runtime.replaceChildren(el('option', { value: '' }, 'Provider default'), ...names.map(name => el('option', { value: name }, name)), ...(initial.runtime && !names.includes(initial.runtime) ? [el('option', { value: initial.runtime }, `${initial.runtime} (previously reviewed)`)] : []));
    runtime.value = initial.runtime || '';
  } catch (error) { if (p.current()) feedback.replaceChildren(message('Runtime choices could not be read. The current selection is preserved for server validation.', 'warning'), problem(error)); }
  if (p.current()) await readComponents();
}

export async function installReview(p, entry, node, plan, options, back) {
  const body = acceptedInstallOptions(plan, options), feedback = el('div', { 'aria-live': 'polite' });
  let verifying = false;
  const again = () => installReview(p.panel.view('Review install plan'), entry, node, plan, options, back);
  const execute = step => { if (!verifying && plan.plan_id) return providerOperation(p, entry, node, { label: step === undefined ? 'Install provider' : `Execute install step ${step}`, suffix: step === undefined ? '/install' : '/install/execute-step', body: step === undefined ? body : { ...body, step }, confirm: `${step === undefined ? 'Run every step in this plan' : `Run step ${step}`} for ${entry.name} on ${node}?` }, back, again); };
  const run = button('Run all steps', () => execute(), 'primary'); run.disabled = !plan.plan_id; run.setAttribute('data-protected-actions', '');
  const verifyAll = button('Verify installation', () => verify());
  const executionButtons = [run], verificationButtons = [verifyAll];
  const script = plan.script_export?.script;
  p.root.append(providerName(entry.key, entry.name, 'h2'), facts([['Node', node], ['Runtime', plan.provider], ['Version', plan.version || 'Resolved by provider'], ['Action', plan.action], ['Install directory', plan.install_dir], ['Components', plan.resolved_install?.selected_components?.join(', ') || 'None reported']]),
    text('Execution is bound to this plan ID, version, runtime and component selection. A changed server plan requires a fresh preview.', 'muted'),
    plan.plan_id ? copyField('Plan ID', plan.plan_id) : message('This response has no plan ID. Execution is unavailable; request a fresh plan.', 'warning'),
    actions(run, verifyAll, leaveButton('Edit options', () => installProvider(p.panel.view('Install provider'), entry, node, back, { ...options, runtime: plan.provider, version: plan.version, ...(plan.resolved_install?.selected_components ? { components: plan.resolved_install.selected_components } : {}) }), p.api), leaveButton('Back', back, p.api)), feedback);
  if (script) p.root.append(el('details', {}, el('summary', {}, 'Reported command script'), text('POSIX shell commands stop on the first failed step. Installation verification and activation remain owned by the managed installer.', 'muted'), commandCopy(script)));
  else p.root.append(text(plan.script_export?.reason || 'This node did not provide a command script. Use managed execution.', 'muted'));
  for (const step of plan.steps || []) {
    const state = plan.current_state?.find(value => value.step === step.step), result = el('div', { 'aria-live': 'polite' });
    const control = button('Run this step', () => execute(step.step)); control.disabled = !plan.plan_id; control.setAttribute('data-protected-actions', ''); executionButtons.push(control);
    const verifyStep = button('Verify step', () => verify(step.step, result)); verificationButtons.push(verifyStep);
    p.root.append(el('section', { class: 'access-section' }, el('h3', {}, `${step.step}. ${step.description}`), state ? text(`Before this plan: ${state.installed ? 'present' : 'not present'}${state.detail ? ' · ' + state.detail : ''}`, 'muted') : null,
      step.notes ? text(step.notes) : null, step.command ? el('details', {}, el('summary', {}, 'Reported command'), installContext(step).length ? [message('This command requires installer context. Use Run this step for managed execution.'), facts(installContext(step))] : null, commandCopy(step.command)) : text('This step is performed by the managed installer.', 'muted'), actions(control, verifyStep), result));
  }
  async function verify(step, target = feedback) {
    if (verifying || !p.current() || !p.api.canChange()) return;
    verifying = true; for (const control of [...executionButtons, ...verificationButtons]) control.disabled = true;
    target.replaceChildren(message('Verifying the current installation…'));
    try {
      const value = await p.api.data(providerPath(entry.key, node, step === undefined ? '/install/verify' : '/install/verify-step'), {
        method: 'POST', effectful: false, body: step === undefined ? { runtime: plan.provider, version: plan.version } : { ...body, step }, signal: p.scope.signal, timeoutMs: 120000 });
      if (!p.current()) return;
      target.replaceChildren(...(step === undefined ? [message(value.all_ok ? 'Reported installation checks passed.' : 'Installation checks need attention.', value.all_ok ? '' : 'warning'), ...(value.steps || []).map(verificationResult)] : [verificationResult(value)]));
    } catch (error) { if (p.current()) target.replaceChildren(problem(error)); }
    finally { verifying = false; if (p.current()) { for (const control of executionButtons) control.disabled = !plan.plan_id; for (const control of verificationButtons) control.disabled = false; } }
  }
}

function commandCopy(command) {
  const output = el('pre', { class: 'log-output', tabindex: '0' }, command), status = el('small', { role: 'status' });
  return el('div', {}, output, actions(button('Copy command', async () => {
    try { await navigator.clipboard.writeText(command); status.textContent = 'Copied.'; }
    catch { output.focus(); status.textContent = 'Copy is unavailable. Select the displayed command to copy it.'; }
  })), status);
}
