import { Lifetime } from './api.mjs';
import { el, svg, text, button, leaveButton, actions, field, input, select, facts, badge, message, problem, formFeedback, invalidField, editorActions, date } from './dom.mjs';
import { formSubmission } from './details.mjs';
import { parameterSettings, providerSchemaPath } from './parameters.mjs';
import { startRun } from './run-form.mjs';
import { inspectWorkload } from './workload-details.mjs';
import { runLogs } from './run-logs.mjs';
import { jobSubject, ownedPath, readWorkloadPages, readyRun, workloadKey, workloadPageSize } from './workload-data.mjs';

export const calibrationDiscovery = async (p, signal = p.scope.signal) => (await p.api.data('', { signal }))?.calibration;
export const canCalibrate = (run, config) => readyRun(run) && Boolean(run.endpoint) && Array.isArray(config?.workload_kinds) && config.workload_kinds.includes(run.endpoint);
const runName = run => run.model || run.provider || run.run_id || run.id;
// Display the server's default request count for a step.
const requestsAt = concurrency => concurrency ? Math.max(3 * concurrency, 10) : 0;
const measured = (value, unit = '') => value == null ? 'Unknown' :
  typeof value === 'number' ? value.toLocaleString(undefined, { maximumFractionDigits: 2 }) + unit : String(value);
const pairs = object => Object.entries(object || {}).map(([key, value]) => [key, measured(value)]);
const nodeList = p => p.api.list('/nodes', { signal: p.scope.signal });
const openRun = (p, node, id) => inspectWorkload(p.panel.view('Run details'), 'runs', node, id);
const openJob = (p, node, id) => inspectWorkload(p.panel.view('Calibration details'), 'jobs', node, id);

function editStored(p, run, back, parameters) {
  return parameterSettings(p, { provider: run.parameters_status?.provider || run.provider, node: run.node,
    model: run.model, endpoint: run.endpoint || '', back, parameters });
}

export function changeRunSettings(p, run, back, { before = () => true, onReplacement } = {}) {
  const begin = action => () => { if (before()) return action(); };
  return el('details', { class: 'access-section' }, el('summary', {}, 'Change settings'), actions(
    run.model ? leaveButton('Save for this model on this node', begin(() => editStored(p, run, back)), p.api) : null,
    leaveButton('Try on this run only', begin(async () => {
      const owner = p.panel.view('Start run');
      owner.root.append(message('Reading compute nodes…'));
      try {
        const nodes = await nodeList(owner);
        if (!owner.current()) return;
        owner.root.replaceChildren();
        const onReady = onReplacement && ((ready, feedback, replace) => {
          if (!replace) return;
          if (!ready.target_revision) feedback.append(message('The replacement run did not report a revision.', 'warning'));
          else if (ready.id !== run.id || ready.target_revision !== run.target_revision) return onReplacement(ready);
        });
        await startRun(owner, nodes, run.node, { model: run.model, provider: run.parameters_status?.provider || run.provider }, { run, back, onReady });
      } catch (error) { if (owner.current()) owner.root.replaceChildren(problem(error), leaveButton('Back', back, owner.api)); }
    }), p.api)));
}

function table(label, columns, rows) {
  return el('div', { class: 'table-wrap calibration-table', tabindex: '0', role: 'region', 'aria-label': label },
    el('table', { 'aria-label': label },
      el('thead', {}, el('tr', {}, columns.map(value => el('th', { scope: 'col' }, value)))),
      el('tbody', {}, rows.map(([name, ...cells]) => el('tr', {}, el('th', { scope: 'row' }, name), cells.map(value => el('td', {}, value)))))));
}

function presetTokens(run, preset) {
  const served = run.context_length;
  if (preset === 'long') {
    const output = served > 0 ? Math.max(1, Math.min(256, Math.floor(served / 4))) : 256;
    return [served > 0 ? Math.max(1, served - output) : 2048, output];
  }
  if (preset === 'retrieval') return [2048, 256];
  if (preset === 'embeddings') return [512, 0];
  return [128, 64];
}

export async function calibrationForm(p, { node: initialNode, nodes, run, request, config, replacementID } = {}) {
  p.root.replaceChildren(message('Loading calibration options…'));
  let capabilities;
  try {
    [capabilities, nodes] = await Promise.all([config || calibrationDiscovery(p), nodes || nodeList(p)]);
  } catch (error) { if (p.current()) p.root.replaceChildren(problem(error)); return; }
  if (!p.current()) return;
  const limitKeys = ['max_runs', 'max_steps', 'max_concurrency', 'max_requests_per_step', 'max_step_timeout_seconds', 'default_step_timeout_seconds'];
  if (!Array.isArray(capabilities?.workload_kinds) || !capabilities.workload_kinds.length ||
    !limitKeys.every(key => Number.isSafeInteger(capabilities[key]) && capabilities[key] > 0)) {
    p.root.replaceChildren(message('Calibration limits are unavailable from this coordinator.', 'warning'));
    return;
  }
  const limits = capabilities;
  const initial = request || { node: initialNode || run?.node, runs: run ? [{ run_id: run.id, target_revision: run.target_revision }] : [] };
  const node = select('calibration_node', [['', 'Choose a compute node…'], ...nodes.map(host => [host.name, host.name])], initial.node);
  node.required = true;
  const number = (name, value, min, max, label) => input(name, value, { type: 'number', min, max, step: 1, 'aria-label': label });
  const timeout = number('step_timeout_seconds', initial.step_timeout_seconds ?? limits.default_step_timeout_seconds, 1, limits.max_step_timeout_seconds);
  const form = el('form', { class: 'entity-editor calibration-form' });
  const draft = p.trackDraft(form), reads = new Lifetime();
  const controls = el('fieldset', { 'data-protected-actions': true });
  const save = el('button', { class: 'primary', type: 'submit' }, 'Start calibration');
  const choices = el('fieldset', { tabindex: '-1' }), workloads = el('div'), grid = el('div'), review = el('div', { 'aria-live': 'polite' });
  const feedback = el('div', { 'aria-live': 'polite' }), results = el('div', { 'aria-live': 'polite' });
  const selected = new Map(), schemas = new Map(), checks = new Map();
  p.scope.signal.addEventListener('abort', () => { reads.end(); selected.clear(); schemas.clear(); }, { once: true });
  let steps = 1, loaded = false, returnRequest;
  const active = () => [...selected.values()].filter(entry => entry.enabled?.checked);
  const submission = formSubmission(p, controls, save, draft);
  const validation = formFeedback(form, feedback, key => {
    const [root, index, group, name, step] = (key || '').replace(/\[(\d+)\]/g, '.$1').split('.');
    const entry = [...selected.values()][Number(index)];
    if (root === 'node') return node;
    if (root === 'step_timeout_seconds') return timeout;
    if (root === 'runs') return group === 'workload' ?
      ({ input_tokens: entry?.input, output_tokens: entry?.output, requests_per_step: entry?.requests,
        concurrency: entry?.cells?.[step ?? 0] })[name] || entry?.enabled : entry?.checkbox || choices;
  }, { inline: true });
  const back = () => calibrationForm(p.panel.view('New calibration'), { request: returnRequest, nodes, config: limits });
  const reselect = (previous, replacement) => {
    const runs = returnRequest.runs.map(item => item.run_id === previous.id ?
      { ...item, run_id: replacement.id, target_revision: replacement.target_revision } : item);
    return calibrationForm(p.panel.view('New calibration'), { request: { ...returnRequest, runs }, nodes, config: limits, replacementID: replacement.id });
  };

  function body() {
    return { node: node.value, ...(timeout.value ? { step_timeout_seconds: Number(timeout.value) } : {}),
      runs: [...selected.values()].map(entry => {
        const { run, enabled, input, output, requests, concurrency } = entry;
        return { run_id: run.id, target_revision: run.target_revision, ...(enabled?.checked ? { workload: {
          input_tokens: Number(input.value), ...(output ? { output_tokens: Number(output.value) } : {}),
          concurrency: Array.from({ length: steps }, (_, step) => Number(concurrency[step] ?? 0)),
          ...(requests.value ? { requests_per_step: Number(requests.value) } : {}),
        } } : {}) };
      }) };
  }

  function setSteps(preset) {
    const entries = active();
    if (!entries.length) return;
    if (preset === 'alone' && entries.length > 1) {
      steps = Math.min(limits.max_steps, entries.length + 1);
      entries.forEach((entry, i) => { entry.concurrency = Array.from({ length: steps }, (_, step) => Number(step === i || step === steps - 1)); });
    } else {
      const peaks = entries.map(({ run }) => 2 * (run.cache?.sequence_slots || run.cache?.max_concurrency || 8));
      steps = Math.min(limits.max_steps, 1 + Math.floor(Math.log2(Math.min(limits.max_concurrency, Math.max(1, ...peaks)))));
      entries.forEach((entry, i) => {
        entry.concurrency = Array.from({ length: steps }, (_, step) =>
          Math.min(2 ** step, Math.max(1, 2 ** Math.floor(Math.log2(peaks[i]))), runLimit(entry.run)));
      });
    }
    draft.touch(); validation.clear(); feedback.replaceChildren(); renderGrid();
  }

  function runLimit(run) {
    return Math.min(limits.max_concurrency, run.max_concurrent_requests || Infinity);
  }

  function renderGrid() {
    const entries = active();
    const counts = Array.from({ length: steps }, () => el('span'));
    const rows = counts.map((count, step) => [String(step + 1), ...entries.map(entry => {
      const label = `Step ${step + 1}, ${runName(entry.run)} concurrency`;
      const control = number(`concurrency_${entry.index}_${step}`, entry.concurrency[step] ?? 0, 0, runLimit(entry.run), label);
      control.required = true;
      entry.cells[step] = control;
      control.addEventListener('input', () => { entry.concurrency[step] = control.value; update(); });
      return control;
    }), count]);
    function update() {
      let total = 0;
      counts.forEach((count, step) => {
        const value = entries.reduce((sum, entry) => {
          const concurrency = Number(entry.concurrency[step]);
          return sum + (concurrency && entry.requests.value ? Number(entry.requests.value) : requestsAt(concurrency));
        }, 0);
        count.textContent = String(value); total += value;
      });
      review.replaceChildren(text(`Total requests: ${total}`), ...[...selected.values()].flatMap(entry => reviewNotices(entry)));
    }
    grid.replaceChildren();
    if (entries.length) grid.append(table('Calibration steps', ['Step', ...entries.map(entry => runName(entry.run)), 'Requests'], rows), actions(
        button('Ramp', () => setSteps('ramp')),
        entries.length > 1 && limits.max_steps > entries.length ? button('Each alone, then together', () => setSteps('alone')) : null,
        steps < limits.max_steps ? button('Add step', () => { steps++; draft.touch(); renderGrid(); entries[0].cells[steps - 1].focus(); }) : null,
        steps > 1 ? button('Remove last step', () => {
          steps--; for (const entry of selected.values()) entry.concurrency.length = steps;
          draft.touch(); renderGrid(); entries[0].cells[steps - 1].focus();
        }) : null));
    for (const entry of selected.values()) entry.updateReview = update;
    update();
  }

  function reviewNotices(entry) {
    const { run, schema, enabled } = entry;
    const notices = [];
    if (run.active_requests > 0) notices.push('Public requests active at observation.');
    if (run.admission_warning?.message) notices.push(run.admission_warning.message);
    const pools = Object.keys(run.memory_commitments_mib || {});
    const host = nodes.find(item => item.name === run.node);
    if (host?.gpu?.unified_memory || pools.includes('unified')) notices.push('Unified memory has no per-process attribution.');
    if (pools.some(pool => ['host', 'ram'].includes(pool))) notices.push('Run reports a host memory commitment, which can include offloaded weights.');
    if (enabled?.checked && schema) {
      const observe = schema.observe;
      const missing = ['running', 'waiting', 'kv_cache_usage'].filter(role => !observe?.[role] ||
        (observe.runtimes?.length && !observe.runtimes.includes(run.runtime)));
      if (missing.length) notices.push(`Engine observations unavailable: ${missing.join(', ')}.`);
      if (run.resolved_parameters?.['disable-log-stats'] === 'true' || run.resolved_parameters?.metrics === 'false') notices.push('Runtime settings disable engine metrics.');
      if (entry.output && !Object.keys(schema.calibration?.output || {}).length) notices.push('Full output length is not forced by this provider.');
    }
    return notices.map(value => text(`${runName(run)}: ${value}`, 'muted'));
  }

  function addRun(run, checkbox, seed) {
    const entry = { run, checkbox, concurrency: [...(seed?.workload?.concurrency || [1])], cells: [], index: run.id };
    selected.set(run.id, entry);
    const served = el('small', { role: 'status' });
    const evidence = el('div'), readProblems = el('div'), resolved = el('div'), settings = el('div'), workload = el('div');
    const renderEvidence = () => {
      const current = entry.run;
      evidence.replaceChildren(facts([['Provider', current.provider], ['Endpoint', current.endpoint || 'Unknown'], ['Pools with commitments', Object.keys(current.memory_commitments_mib || {}).join(', ') || 'Unknown'],
        ['Pinned', current.pinned ? 'Yes' : 'No'], ['Active requests', current.active_requests ?? 0], ['Settings status', current.parameters_status?.state || 'Unknown']]));
      resolved.replaceChildren(facts(pairs(current.resolved_parameters)));
    };
    const showSettings = () => {
      const current = entry.run;
      if (entry.changed) return;
      if (readyRun(current)) settings.append(changeRunSettings(p, current, back, {
        before: () => {
          if (!draft.discard()) return false;
          returnRequest = body();
          return true;
        },
        onReplacement: ready => reselect(current, ready),
      }));
    };
    if (canCalibrate(run, limits)) {
      const embeds = run.endpoint === 'embeddings';
      const presets = embeds ? [['embeddings', 'Embeddings']] : [['short', 'Short chat'], ['long', 'Long context'], ['retrieval', 'Retrieval']];
      const preset = select('workload_preset', [['', 'Custom'], ...presets], seed?.workload ? '' : presets[0][0]);
      const [suggestedInput, suggestedOutput] = presetTokens(run, presets[0][0]);
      const { input_tokens = suggestedInput, output_tokens = suggestedOutput, requests_per_step = '' } = seed?.workload || {};
      entry.enabled = input('calibration_workload', '', { type: 'checkbox', checked: !seed || !!seed.workload });
      entry.input = number('input_tokens', input_tokens, 1);
      entry.output = embeds ? null : number('output_tokens', output_tokens, 1);
      entry.requests = number('requests_per_step', requests_per_step, 1, limits.max_requests_per_step);
      const fields = el('fieldset', {}, field('Workload preset', preset), field('Input tokens', entry.input),
        entry.output ? field('Output tokens', entry.output) : null, served, field('Requests per step (optional)', entry.requests));
      entry.input.required = true;
      if (entry.output) entry.output.required = true;
      const update = () => {
        const context = entry.run.context_length;
        const sum = Number(entry.input.value) + Number(entry.output?.value || 0);
        const tooLong = context > 0 && sum > context;
        served.textContent = context > 0 ? `${sum} of ${context} served tokens${tooLong ? ' (exceeds served context)' : ''}` : 'Served context is unknown.';
        entry.input.setCustomValidity?.(tooLong ? 'Requested tokens exceed served context.' : '');
        entry.updateReview?.();
      };
      entry.checkTokens = update;
      for (const control of [entry.input, entry.output, entry.requests].filter(Boolean)) control.addEventListener('input', () => { preset.value = ''; update(); });
      preset.addEventListener('change', () => {
        if (!preset.value) return;
        const [prompt, output] = presetTokens(entry.run, preset.value);
        entry.input.value = prompt;
        if (entry.output) entry.output.value = output;
        update();
      });
      const toggle = () => { fields.disabled = fields.hidden = !entry.enabled.checked; renderGrid(); };
      entry.enabled.addEventListener('change', toggle);
      workload.append(field('Send calibration traffic', entry.enabled), fields);
      toggle(); update();
    } else workload.append(text('Resident only. No workload kind is available for this run.', 'muted'));
    entry.root = el('section', { class: 'access-section' }, el('h3', {}, runName(run)), evidence, readProblems,
      el('details', {}, el('summary', {}, 'Settings'), resolved), settings, workload);
    workloads.append(entry.root);
    renderEvidence();
    const signal = AbortSignal.any([p.scope.signal, checks.get(run.id).signal]);
    const current = () => p.current() && !signal.aborted;
    if (!readyRun(run)) return;
    if (!schemas.has(run.provider)) schemas.set(run.provider,
      p.api.request('/zzrouter/v1' + providerSchemaPath(run.provider), { signal: p.scope.signal }).catch(error => ({ error })));
    schemas.get(run.provider).then(schema => {
      if (!current()) return;
      entry.schema = schema.error ? undefined : schema;
      if (schema.error) readProblems.append(problem(schema.error));
      entry.updateReview?.();
    });
    p.api.data(ownedPath('runs', run.node, run.id), { signal }).then(detail => {
      if (!current()) return;
      if (detail.target_revision !== run.target_revision || !readyRun(detail)) {
        entry.changed = true;
        throw new Error('Run changed while reading settings. Refresh the run list, then select it again.');
      }
      entry.run = { ...detail, node: run.node };
      renderEvidence(); entry.checkTokens?.(); entry.updateReview?.();
    }).catch(error => { if (current()) readProblems.append(problem(error)); }).finally(() => { if (current()) showSettings(); });
  }

  async function loadRuns(seed = [], keep = false) {
    const scope = reads.next();
    if (!keep) {
      for (const controller of checks.values()) controller.abort();
      checks.clear(); selected.clear(); choices.replaceChildren(el('legend', {}, 'Ready runs')); workloads.replaceChildren();
      steps = Math.min(limits.max_steps, Math.max(1, ...seed.map(item => item.workload?.concurrency?.length || 0)));
      loaded = false; renderGrid(); feedback.replaceChildren();
    }
    results.replaceChildren();
    if (!node.value) return;
    if (nodes.find(host => host.name === node.value)?.health_status === 'down') {
      results.append(message('This node is offline.', 'warning')); return;
    }
    const signal = AbortSignal.any([p.scope.signal, scope.signal]), name = node.value;
    try {
      const runs = (await p.api.list(ownedPath('runs', name), { signal })).filter(readyRun);
      if (!p.current() || !scope.current()) return;
      const listed = runs.map(run => ({ ...run, node: name }));
      for (const item of seed) if (!listed.some(run => run.id === item.run_id)) listed.push({ id: item.run_id, node: name });
      for (const entry of selected.values()) {
        const fresh = listed.find(run => run.id === entry.run.id);
        if (fresh?.target_revision !== entry.run.target_revision) entry.changed = true;
        if (!fresh) listed.push(entry.run);
      }
      choices.replaceChildren(el('legend', {}, 'Ready runs'));
      const restore = new Map();
      for (const [index, run] of listed.entries()) {
        const prior = selected.get(run.id);
        const initial = seed.find(item => item.run_id === run.id);
        const checkbox = input(`calibration_run_${index}`, '', { type: 'checkbox', checked: !!prior || !!initial });
        if (prior) prior.checkbox = checkbox;
        const change = () => {
          if (checkbox.checked && selected.size >= limits.max_runs) {
            checkbox.checked = false;
            queueMicrotask(() => { if (p.current()) validation.error(invalidField(`Choose up to ${limits.max_runs} runs.`, checkbox)); });
            return;
          }
          if (checkbox.checked) {
            checks.set(run.id, new AbortController()); addRun(run, checkbox, request ? initial : undefined);
          } else {
            checks.get(run.id)?.abort(); checks.delete(run.id);
            selected.get(run.id)?.root.remove(); selected.delete(run.id);
          }
          renderGrid();
        };
        checkbox.addEventListener('change', change);
        const label = [runName(run), run.provider || 'Unavailable', run.endpoint, prior?.changed ? 'Selection changed' : ''].filter(Boolean).join(' · ');
        choices.append(field(label, checkbox));
        if (initial && !prior) restore.set(run.id, () => {
          change();
          const entry = selected.get(run.id);
          if (entry && (!request || replacementID) && initial.target_revision && initial.target_revision !== run.target_revision) {
            entry.changed = true;
            results.append(message('The run changed before the form opened. Select it again to use the current revision.', 'warning'));
          }
        });
      }
      for (const item of seed) restore.get(item.run_id)?.();
      loaded = true;
      if (!runs.length) results.append(message('No ready runs on this node.'));
    } catch (error) { if (p.current() && scope.current()) results.replaceChildren(problem(error)); }
  }

  form.addEventListener('input', () => feedback.replaceChildren());
  node.addEventListener('change', () => loadRuns());
  form.addEventListener('submit', event => {
    event.preventDefault();
    if (!loaded || !form.reportValidity()) return;
    const submitted = body(), entries = active();
    const badRun = [...selected.values()].find(entry => entry.changed || !entry.run.target_revision || !readyRun(entry.run));
    if (badRun || !entries.length) {
      validation.error(invalidField(badRun ? 'Choose a current ready run with a target revision.' : 'Choose at least one run with a workload.', badRun?.checkbox || choices)); return;
    }
    for (let step = 0; step < steps; step++) {
      if (!entries.some(entry => Number(entry.concurrency[step]) > 0)) {
        validation.error(invalidField('At least one run must be active in each step.', entries[0].cells[step])); return;
      }
    }
    const operation = crypto.randomUUID();
    void submission.run(async () => {
      const result = await p.api.data('/calibrations', { method: 'POST', body: submitted, idempotencyKey: operation, signal: p.scope.signal });
      if (!result?.job_id || result.node !== submitted.node) throw Object.assign(new Error('The calibration response did not identify a job on the submitted node.'), { uncertain: true });
      return result;
    }, result => openJob(p, result.node, result.job_id), (error, blocked) => {
      const runs = [...selected.values()];
      const affected = error.run_id ? selected.get(error.run_id) : runs.length === 1 ? runs[0] : undefined;
      const control = error.code === 'calibration_running' ? node : {
        context_too_small: affected?.input, concurrency_above_limit: affected?.cells?.[0],
        calibration_unsupported: affected?.enabled || affected?.checkbox, run_not_found: affected?.checkbox,
        run_not_ready: affected?.checkbox, run_changed: affected?.checkbox,
      }[error.code];
      validation.error(!error.errors?.length && control ? Object.assign(error, { field: control }) : error);
      if (error.job_id) feedback.append(leaveButton('View calibration job', () => openJob(p, submitted.node, error.job_id), p.api));
      if (blocked) feedback.append(facts([['Node', submitted.node], ['Operation ID', operation]]),
        leaveButton('Inspect calibrations', () => { draft.clean(); return p.go('workloads', workloadKey('node', submitted.node, ''), 'calibrations'); }, p.api));
    });
  });
  controls.append(field('Compute node', node), results, leaveButton('Refresh ready runs', () =>
    loadRuns(loaded ? [] : initial.node === node.value ? initial.runs : [], loaded), p.api), choices, workloads,
  el('h3', {}, 'Steps'), grid, field('Step timeout (seconds)', timeout),
  el('h3', {}, 'Review'), review);
  form.append(feedback, controls, editorActions(save, p.close, p.api, draft));
  p.root.replaceChildren(el('h2', {}, 'New calibration'));
  if (replacementID) p.root.append(message(`Selected replacement run ${replacementID}.`));
  p.root.append(form);
  await loadRuns(initial.runs);
}

export function calibrationJob(p, job, back) {
  const meta = job.meta || {};
  const header = el('div', {}, facts([['Node', job.node], ['Job ID', job.job_id],
    ['Latest event', date(job.at, 'Unknown')], ['Retention deadline', date(job.expires_at, 'Unknown')]]));
  if (meta.report) {
    header.append(calibrationReport(p, job, back));
    return header;
  }
  const stage = { checking: 'Checking runs', warming_up: 'Warming up', measuring: 'Measuring' }[job.step] || job.phase;
  const requests = new Set([...Object.keys(meta.requests_completed || {}), ...Object.keys(meta.requests_total || {})]);
  header.prepend(el('h2', {}, 'Calibration'), badge(stage));
  if (Number.isInteger(meta.step) && Number.isInteger(meta.steps)) header.append(text(`Step ${meta.step + 1} of ${meta.steps}`));
  for (const id of requests) header.append(text(`${id}: ${measured(meta.requests_completed?.[id])} / ${measured(meta.requests_total?.[id])} requests completed`));
  if (meta.sample) header.append(el('section', {}, el('h3', {}, 'Latest resource sample'),
    facts([['Sample time', date(meta.sample.at, 'Unknown')]]), (meta.sample.pools || []).map(livePool)));
  if (job.err) header.append(message(job.err, 'error'));
  if (job.warning) header.append(message(job.warning, 'warning'));
  if (['done', 'failed'].includes(job.phase)) header.append(message('No calibration report was returned.', 'warning'));
  return header;
}

function livePool(pool) {
  const system = [['Available', measured(pool.available_mib, ' MiB')], ['Swap used', measured(pool.swap_used_mib, ' MiB')]];
  const card = (title, entries) => el('section', { class: 'card' }, el('h4', {}, title), facts(entries));
  switch (pool.kind) {
    case 'gpu': return card(pool.id, [
      ['Used / total', `${measured(pool.used_mib, ' MiB')} / ${measured(pool.total_mib, ' MiB')}`],
      ['Utilization', measured(pool.utilization_pct, '%')], ['Power', measured(pool.power_w, ' W')],
    ]);
    case 'unified': return card('Unified memory', system);
    case 'host': return card('Host memory', [...system, ['CPU', measured(pool.cpu_pct, '%')]]);
    default: return null;
  }
}

function resourceCard(pool, report) {
  const gpu = pool.kind === 'gpu';
  const runs = new Map(report.runs.map(run => [run.run_id, runName(run)]));
  const holders = pool.holders?.map(holder => {
    const owner = runs.get(holder.run_id);
    const name = owner || (holder.run_id ? `Other run ${holder.run_id}` : 'Other process');
    return text(`${name} · PID ${measured(holder.pid)} · ${measured(holder.memory_mib, ' MiB')}`);
  });
  const title = pool.kind === 'unified' ? 'Unified memory' : gpu ? pool.id : 'Host memory';
  return el('section', { class: 'card' }, el('h4', {}, title), facts([['Total', measured(pool.total_mib, ' MiB')],
    ...(gpu ? [['Vendor', measured(pool.vendor)], ['Ownership', measured(pool.ownership)]] : [])]), holders,
    gpu && pool.ownership === 'shared' && !holders?.length ? text('Other holder identity is unknown.') : null,
    pool.kind === 'unified' ? text('Per-process attribution is unavailable.', 'muted') : null,
    report.steps.flatMap(step => (step.resources || []).filter(resource => resource.id === pool.id).map(resource => {
      const peak = (label, value, unit) => [label, `${measured(value?.mean, unit)} / ${measured(value?.peak, unit)}`];
      return el('div', {}, el('h5', {}, `Step ${step.index + 1}`), facts(gpu ?
        [['Peak used / total', `${measured(resource.peak_used_mib, ' MiB')} / ${measured(pool.total_mib, ' MiB')}`],
          ['Minimum free', measured(resource.minimum_free_mib, ' MiB')], peak('Utilization mean / peak', resource.utilization_pct, '%'),
          peak('Power mean / peak', resource.power_w, ' W'), ['Other holders at peak', measured(resource.other_holders_peak_mib, ' MiB')]] :
        [['Minimum available', measured(resource.minimum_available_mib, ' MiB')], ['Peak swap used', measured(resource.peak_swap_used_mib, ' MiB')],
          ...(pool.kind === 'host' ? [peak('CPU mean / peak', resource.cpu_pct, '%')] : [])]));
    })));
}

const perUserRate = row => row.inter_token_latency_ms?.p50 > 0 ? 1000 / row.inter_token_latency_ms.p50 : null;
const outputRate = row => row.throughput?.output_tokens_per_second;
function stepTable(report) {
  const names = new Map(report.runs.map(run => [run.run_id, runName(run)]));
  const label = step => `Step ${step.index + 1}`;
  const percentile = metric => `${measured(metric?.p50)} / ${measured(metric?.p99)}`;
  const rows = report.steps.flatMap(step => [
    ...step.runs.map(row => [`${label(step)} · ${names.get(row.run_id) || row.run_id}`, measured(row.concurrency),
      badge(row.outcome || 'Unknown'), measured(outputRate(row)), measured(perUserRate(row)),
      measured(row.throughput?.input_tokens_per_second), measured(row.throughput?.requests_per_second),
      percentile(row.time_to_first_token_ms), percentile(row.inter_token_latency_ms),
      measured(row.engine?.kv_cache_usage_peak), Object.keys(row.memory_peak_mib || {}).length ?
        pairs(row.memory_peak_mib).map(([pool, value]) => text(`${pool}: ${value === 'Unknown' ? value : value + ' MiB'}`)) : 'Unknown',
      measured(row.cpu_pct?.peak, '%'), measured(row.public_requests)]),
    ...(report.runs.length > 1 ? [[`${label(step)} · Total`, ' ', badge(step.outcome || 'Unknown'), measured(outputRate(step)),
      ...Array(9).fill('')]] : []),
  ]);
  return rows.length ? table('Measured calibration steps', ['Step / run', 'Concurrency', 'Outcome', 'Output tok/s', 'Per-user tok/s at p50 ITL',
    'Input tok/s', 'Requests/s', 'TTFT p50 / p99 (ms)', 'ITL p50 / p99 (ms)', 'KV cache peak (fraction)', 'Memory peak', 'CPU peak', 'Public requests'], rows) :
    text('No measured steps.');
}

function throughputChart(report, run) {
  const points = report.steps.flatMap(step => {
    const row = step.runs.find(row => row.run_id === run.run_id);
    if (!row) return [];
    const total = outputRate(row), user = perUserRate(row);
    return Number.isFinite(total) && Number.isFinite(user) ? [{ total, user, step: step.index + 1 }] : [];
  });
  if (!points.length) return null;
  const maxX = Math.max(1, ...points.map(point => point.total));
  const maxY = Math.max(1, ...points.map(point => point.user));
  const x = point => 45 + 240 * point.total / maxX, y = point => 140 - 110 * point.user / maxY;
  return el('figure', { class: 'calibration-chart' }, el('figcaption', {}, `${runName(run)} throughput`),
    svg('svg', { viewBox: '0 0 320 180', role: 'img', 'aria-label': 'Per-user output tokens per second versus total output tokens per second across steps' },
      svg('path', { d: 'M45 30 V140 H285', fill: 'none', stroke: 'currentColor' }),
      svg('polyline', { points: points.map(point => `${x(point)},${y(point)}`).join(' '), fill: 'none', stroke: 'currentColor' }),
      points.map(point => svg('circle', { cx: x(point), cy: y(point), r: 4, fill: 'currentColor' },
        svg('title', {}, `Step ${point.step}: ${measured(point.user)} per user, ${measured(point.total)} total`))),
      svg('text', { x: 0, y: 15 }, 'Per-user tok/s'), svg('text', { x: 41, y: 34, 'text-anchor': 'end' }, measured(maxY)),
      svg('text', { x: 42, y: 154 }, '0'), svg('text', { x: 285, y: 154, 'text-anchor': 'end' }, measured(maxX)),
      svg('text', { x: 165, y: 174, 'text-anchor': 'middle' }, 'Total tok/s')));
}

function settingsTested(report, other, action) {
  return report.runs.map(run => {
    const sameTarget = candidate => ['provider', 'model', 'endpoint'].every(key => candidate[key] === run[key]);
    const match = other?.runs.find(candidate => candidate.run_id === run.run_id) || other?.runs.find(sameTarget);
    const parameters = run.resolved_parameters || {};
    const keys = [...new Set([...Object.keys(parameters), ...Object.keys(match?.resolved_parameters || {})])].sort();
    return el('section', { class: 'access-section' }, el('h4', {}, runName(run)), facts([['Settings status', measured(run.parameters_status)],
      ['Provider', run.provider], ['Endpoint', run.endpoint], ['Served context', measured(run.served_context_tokens, ' tokens')],
      ['Output forced', measured(run.output_forced)], ['Engine roles', run.observed_roles?.join(', ') || 'None reported'],
      ...keys.map(key => [key, other && (!match || parameters[key] !== match.resolved_parameters?.[key]) ?
        el('mark', { class: 'calibration-difference' }, measured(parameters[key]), ' (different)') : measured(parameters[key])])]), action?.(run));
  });
}

function calibrationReport(p, job, back) {
  const report = job.meta.report;
  const rerun = () => calibrationForm(p.panel.view('New calibration'), { request: report.request });
  const revisit = () => back ? back() : openJob(p, job.node, job.job_id);
  const next = report.reason?.next_action;
  const nextActions = {
    zz_params_set: ['Change model settings', run => editStored(p, { ...run, node: job.node }, revisit)],
    zz_runs: ['View run details', run => openRun(p, job.node, run.run_id)],
    zz_logs: ['View runtime logs', run => runLogs(p.panel.view('Runtime logs'), { node: job.node, id: run.run_id, back: revisit })],
    zz_providers: ['View provider details', run => p.go('providers', `local/${run.provider}`, undefined, job.node)],
    zz_calibrate: ['Edit workload', rerun],
  };
  let nextControl;
  const [label, follow] = nextActions[next?.tool] || [];
  if (follow) {
    const affected = new Set(report.steps.flatMap(step => step.runs.filter(row => row.reason?.code === report.reason.code).map(row => row.run_id)));
    const matches = report.runs.filter(run => affected.has(run.run_id));
    const candidates = matches.length ? matches : report.runs;
    const run = candidates[0];
    const choose = next.tool !== 'zz_calibrate' && candidates.length > 1 ?
      select('reason_run', candidates.map(run => [run.run_id, runName(run)]), run.run_id) : null;
    nextControl = actions(choose ? field('Run for next action', choose) : null,
      leaveButton(label, () => follow(choose ? candidates.find(run => run.run_id === choose.value) : run), p.api));
  }
  const save = run => {
    const redacted = new Set(run.redacted_parameters || []), omitted = [];
    const overrides = Object.fromEntries((run.overridden || []).flatMap(path => {
      if (path.startsWith('environment.')) return [];
      const key = path.replace(/^parameters\./, ''), value = run.resolved_parameters?.[key];
      if (redacted.has(key)) { omitted.push(key); return []; }
      return value == null ? [] : [[key, value]];
    }));
    const action = Object.keys(overrides).length ? leaveButton('Save as model settings on this node',
      () => editStored(p, { ...run, node: job.node }, revisit, overrides), p.api) :
      run.overridden?.length ? text('Override values are unavailable for saving.') : null;
    return [action, omitted.length ? text(`Settings prefill omitted redacted values for ${omitted.join(', ')}.`) : null];
  };
  return el('div', { class: 'calibration-report' }, el('h2', {}, 'Calibration report'), badge(report.outcome),
    report.reason ? message(report.reason.message || report.reason.code, 'warning') : null, nextControl,
    actions(leaveButton('Run again', rerun, p.api), leaveButton('Compare with…', () => compare(p.panel.view('Compare calibrations'), job, revisit), p.api)),
    el('h3', {}, 'Resources'), report.resources.map(pool => resourceCard(pool, report)),
    el('h3', {}, 'Measured steps'), stepTable(report), report.runs.map(run => throughputChart(report, run)),
    el('h3', {}, 'Settings tested'), settingsTested(report, undefined, save));
}

async function compare(p, job, back) {
  const reads = new Lifetime(), candidates = new Map();
  let offset = 0, more = true;
  const feedback = el('div', { 'aria-live': 'polite' }), result = el('div', { class: 'calibration-compare' });
  const choose = select('comparison_job', [], '');
  const older = button('Load older calibrations', load);
  const placeholder = () => el('option', { value: '' }, 'Choose a retained calibration…');
  p.root.append(el('h2', {}, 'Compare calibrations'), facts([['Node', job.node]]), leaveButton('Back to report', back, p.api),
    field('Other calibration', choose), older, feedback, result);
  p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  async function load() {
    if (!more || older.disabled) return;
    older.disabled = true;
    const [page] = await readWorkloadPages(p, [job.node], 'jobs', new Map([[job.node, offset]]), 'calibration');
    if (!p.current()) return;
    older.disabled = false;
    if (page.error) { feedback.replaceChildren(problem(page.error)); return; }
    more = page.more; offset += workloadPageSize; older.hidden = !more;
    const selected = choose.value;
    for (const row of page.rows) {
      if (row.job_id !== job.job_id && row.kind === 'calibration' && ['done', 'failed'].includes(row.phase)) candidates.set(row.job_id, row);
    }
    choose.replaceChildren(placeholder(), ...[...candidates.values()].map(row => el('option', { value: row.job_id }, `${jobSubject(row)} · ${date(row.at, 'Unknown')}`)));
    choose.value = selected;
    feedback.replaceChildren();
    if (!candidates.size && !more) feedback.append(message('No other retained calibrations on this node.'));
  }
  choose.addEventListener('change', async () => {
    const scope = reads.next(), id = choose.value;
    if (!id) { result.replaceChildren(); return; }
    feedback.replaceChildren(message('Reading the retained report…')); result.replaceChildren();
    try {
      const other = await p.api.data(ownedPath('jobs', job.node, id), { signal: AbortSignal.any([scope.signal, p.scope.signal]) });
      if (!p.current() || !scope.current()) return;
      if (other.kind !== 'calibration' || !other.meta?.report) throw new Error('The selected job has no retained calibration report.');
      const original = job.meta.report, comparison = other.meta.report;
      const column = (id, report, against) => el('section', {}, el('h3', {}, id), badge(report.outcome),
        stepTable(report), settingsTested(report, against));
      result.replaceChildren(column(job.job_id, original, comparison), column(id, comparison, original));
      feedback.replaceChildren();
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error)); }
  });
  await load();
}
