import test from 'node:test';
import assert from 'node:assert/strict';
import { API, APIError } from '../api.mjs';
import { calibrationForm, calibrationJob } from '../calibrations.mjs';
import { details } from '../details.mjs';
import { inspectDeployment } from '../deployments.mjs';
import { startRun } from '../run-form.mjs';
import { inspectWorkload } from '../workload-details.mjs';
import { workloads } from '../workloads.mjs';
import { workloadKey } from '../workload-data.mjs';
import { installDOM, named, button, find, drain, walk } from './dom-fixture.mjs';

const config = { workload_kinds: ['chat', 'embeddings'], max_runs: 3, max_steps: 8, max_concurrency: 32,
  max_requests_per_step: 128, max_step_timeout_seconds: 900, default_step_timeout_seconds: 300 };
const node = 'gpu/1';
const qwen = { id: 'qwen', node, status: 'running', model: 'org/qwen', provider: 'engine', endpoint: 'chat', target_revision: 'q1',
  context_length: 2048, active_requests: 2, cache: { sequence_slots: 3 }, memory_commitments_mib: { 'GPU-1': 512, host: 256 },
  parameters: { slots: '3' }, environment: { SLOT_DEVICE: 'synthetic-private' }, resolved_parameters: { slots: '3' },
  parameters_status: { state: 'stale', provider: 'engine' }, pinned: true };
const embed = { ...qwen, id: 'embed', target_revision: 'e1', model: 'org/embed', endpoint: 'embeddings', context_length: 4096 };
const service = { id: 'ui', node, status: 'running', provider: 'service', target_revision: 's1' };
const extras = { ...qwen, id: 'extra', target_revision: 'x1', model: 'org/extra' };
const nodes = [{ name: node, health_status: 'healthy', providers: [{ run_lifecycle: 'process' }] }, { name: 'offline', health_status: 'down' }];
const schema = { parameters: { slots: { type: 'int', min: 1, max: 32, description: 'Engine slots' } }, environment: { SLOT_DEVICE: { type: 'string' } },
  observe: { running: 'running' } };
const report = {
  outcome: 'verified', reason: null,
  request: { node, step_timeout_seconds: 411, runs: [{ run_id: qwen.id, target_revision: 'historic',
    workload: { input_tokens: 128, output_tokens: 64, concurrency: [1], requests_per_step: 11 } }] },
  resources: [{ id: 'GPU-1', kind: 'gpu', total_mib: 1000, ownership: 'shared', holders: [
    { pid: 40, run_id: 'qwen', memory_mib: 200 }, { pid: 42, run_id: 'sidecar', memory_mib: null },
  ] }, { id: 'host', kind: 'host', total_mib: null }],
  runs: [{ run_id: 'qwen', model: qwen.model, provider: qwen.provider, endpoint: 'chat', parameters_status: 'stale', overridden: ['parameters.slots', 'environment.SLOT_DEVICE'],
    resolved_parameters: { slots: '3' }, output_forced: false, observed_roles: ['running'], served_context_tokens: 2048 },
  { run_id: 'embed', model: embed.model, provider: embed.provider, endpoint: 'embeddings', resolved_parameters: {}, overridden: [] }],
  steps: [{ index: 0, outcome: 'verified', throughput: { output_tokens_per_second: 27 },
    resources: [{ id: 'GPU-1', peak_used_mib: null, minimum_free_mib: 499, utilization_pct: { mean: 0, peak: 90 }, power_w: null },
      { id: 'host', minimum_available_mib: 4096, peak_swap_used_mib: 0, cpu_pct: { mean: 2, peak: 4 } }],
    runs: [{ run_id: 'qwen', outcome: 'queued', concurrency: 4, throughput: { output_tokens_per_second: 34 },
      inter_token_latency_ms: { p50: 40, p99: 49 }, time_to_first_token_ms: { p50: null, p99: 340 }, engine: { kv_cache_usage_peak: null },
      memory_peak_mib: { 'GPU-1': 0 }, cpu_pct: { peak: 92 }, public_requests: 0 }] }],
};
const job = { job_id: 'cal/first', node, kind: 'calibration', phase: 'done', seq: 3, epoch: 'one', meta: { report }, expires_at: '2026-11-21T20:00:00Z' };

function fixture(t, overrides = {}) {
  const controller = new AbortController(), calls = [], routes = [];
  t.after(() => controller.abort());
  const root = installDOM(t, { requestAnimationFrame: callback => setTimeout(callback, 0), cancelAnimationFrame: clearTimeout });
  const api = { canLeave: () => true, canChange: () => true };
  for (const method of ['list', 'request', 'data']) api[method] = async (path, options = {}) => {
    calls.push({ path, ...options, transport: method });
    const next = overrides[method]?.(path, options);
    if (next !== undefined) return next;
    if (path === '') return { calibration: config };
    if (path === '/nodes') return nodes;
    if (path.startsWith('/runs?')) return [qwen, embed, service, extras, { ...qwen, id: 'retained', record_only: true }];
    if (path.startsWith('/models?')) return [{ name: qwen.model, node, assigned_app: qwen.provider }];
    if (path.startsWith('/providers?')) return [{ name: qwen.provider, node, enabled: true, run_lifecycle: 'process' }];
    if (path.endsWith('/schema')) return schema;
    if (path.includes('/resolved?')) return { parameters: { slots: { value: '2', tier: 'defaults' } } };
    if (path.includes('/parameters?scope=')) return { parameters: { slots: '2' }, environment: {}, available_endpoints: ['chat', 'embeddings'] };
    if (path.includes('/logs?')) return { lines: ['synthetic log'] };
    if (path.startsWith('/runs/')) return [qwen, embed, service, extras].find(run => path.startsWith('/runs/' + run.id + '?'));
    if (path.startsWith('/jobs/')) return job;
    assert.fail('Unexpected API request ' + path);
  };
  const p = { root, api, current: () => !controller.signal.aborted, scope: { signal: controller.signal },
    go: (...args) => routes.push(args), state: { node_name: 'coord' } };
  const owner = details(p, 'Workloads', () => {});
  return { root, p, owner, api, calls, routes };
}
const click = async (root, label) => { button(root, label).dispatchEvent(new Event('click')); await drain(); };
const change = (control, value, event = 'change') => {
  if (control.type === 'checkbox') control.checked = value;
  else control.value = value;
  control.dispatchEvent(new Event(event));
};
const submit = async root => {
  const form = find(root, el => el.tag === 'form' && el.className?.includes('entity-editor'));
  form.dispatchEvent(new Event('submit', { cancelable: true }));
  await drain();
};
const steps = (root, id) => walk(root).filter(el => el.getAttribute('name')?.startsWith(`concurrency_${id}_`)).map(el => Number(el.value));

test('calibration opens through the shared API and renders required-read failures as Problems', async t => {
  const f = fixture(t);
  const routes = new Map([
    ['/zzrouter/v1', { calibration: config }],
    ['/zzrouter/v1/nodes', nodes],
    ['/zzrouter/v1/runs?node=gpu%2F1', [qwen]],
  ]);
  let fail;
  const requested = [];
  const api = new API({ fetcher: async path => {
    requested.push(path);
    assert.ok(routes.has(path), `Unexpected request ${path}`);
    if (path === fail) return Response.json({ detail: 'Required read failed.', code: 'read_unavailable', request_id: 'fixture-read' }, { status: 503 });
    return Response.json({ data: routes.get(path) });
  } });
  await calibrationForm({ ...f.owner, api }, { node });
  assert.deepEqual(requested, [...routes.keys()]);
  assert.ok(named(f.root, 'calibration_run_0'));
  for (fail of routes.keys()) {
    await calibrationForm({ ...f.owner.panel.view('New calibration'), api }, { node });
    const alert = find(f.root, el => el.getAttribute('role') === 'alert');
    assert.equal(alert.className, 'notice error');
    assert.match(alert.textContent, /Required read failed\..*Technical details.*read_unavailable.*fixture-read/);
  }
});

test('discovery limits, workload presets, editable step presets and request review drive the exact job submission', async t => {
  const f = fixture(t, { data: (path, options) => path === '/calibrations' && options.method === 'POST' ? { job_id: job.job_id, node } : undefined });
  await calibrationForm(f.owner, { run: qwen });
  await drain();
  assert.match(f.root.textContent, /PinnedYesActive requests2Settings statusstale/);
  assert.match(f.root.textContent, /GPU-1, host/);
  assert.match(f.root.textContent, /Public requests active.*host memory commitment.*waiting, kv_cache_usage.*not forced/);
  assert.doesNotMatch(f.root.textContent, /synthetic-private|retained/);
  change(named(f.root, 'workload_preset'), 'long');
  assert.equal(named(f.root, 'input_tokens').value, '1792');
  assert.match(f.root.textContent, /2048 of 2048 served tokens/);
  change(named(f.root, 'workload_preset'), 'retrieval');
  assert.match(f.root.textContent, /exceeds served context/);
  assert.equal(named(f.root, 'input_tokens').validationMessage, 'Requested tokens exceed served context.');
  change(named(f.root, 'workload_preset'), 'short');
  assert.equal(named(f.root, 'input_tokens').validationMessage, '');
  await click(f.root, 'Ramp');
  assert.deepEqual(steps(f.root, 'qwen'), [1, 2, 4]);
  assert.match(f.root.textContent, /Total requests: 32/);
  change(named(f.root, 'calibration_run_1'), true);
  change(named(f.root, 'calibration_run_2'), true);
  change(named(f.root, 'calibration_run_3'), true);
  await drain();
  assert.match(f.root.textContent, /Choose up to 3 runs/);
  assert.equal(named(f.root, 'calibration_run_3').checked, false);
  assert.match(f.root.textContent, /Resident only/);
  assert.equal(walk(f.root).filter(el => el.getAttribute('name') === 'output_tokens').length, 1);
  await click(f.root, 'Each alone, then together');
  assert.deepEqual(steps(f.root, 'qwen'), [1, 0, 1]);
  assert.deepEqual(steps(f.root, 'embed'), [0, 1, 1]);
  const edit = named(f.root, 'concurrency_qwen_2');
  change(edit, 4, 'input');
  assert.match(f.root.textContent, /Total requests: 42/);
  await submit(f.root);
  const start = f.calls.find(call => call.path === '/calibrations');
  assert.ok(start.idempotencyKey);
  assert.deepEqual(start.body, { node, step_timeout_seconds: 300, runs: [
    { run_id: 'qwen', target_revision: 'q1', workload: { input_tokens: 128, output_tokens: 64, concurrency: [1, 0, 4] } },
    { run_id: 'embed', target_revision: 'e1', workload: { input_tokens: 512, concurrency: [0, 1, 1] } },
    { run_id: 'ui', target_revision: 's1' },
  ] });
  assert.match(f.root.textContent, /Calibration report/);
  assert.equal(f.calls.filter(call => call.path === '/calibrations').length, 1);
});

test('field problems link to the exact calibration control and settled submissions get new keys', async t => {
  let issue = { code: 'context_too_small', key: 'runs[0].workload.input_tokens', control: 'input_tokens' };
  const codes = [
    issue, { code: 'concurrency_above_limit', key: 'runs[0].workload.concurrency[0]', control: 'concurrency_qwen_0' },
    { code: 'run_not_found', key: 'runs[0].run_id', control: 'calibration_run_0' },
    { code: 'run_not_ready', key: 'runs[0].run_id', control: 'calibration_run_0' },
    { code: 'run_changed', key: 'runs[0].target_revision', control: 'calibration_run_0' },
    { code: 'calibration_unsupported', key: 'runs[0].workload', control: 'calibration_workload' },
    { code: 'calibration_running', key: 'node', control: 'calibration_node' },
    { code: 'invalid_field', key: 'step_timeout_seconds', control: 'step_timeout_seconds' },
  ];
  const f = fixture(t, { data: path => {
    if (path === '/calibrations') throw new APIError('Check this calibration', { code: issue.code, errors: [{ key: issue.key, message: 'Field refusal' }],
      job_id: issue.code === 'calibration_running' ? job.job_id : undefined });
  } });
  await calibrationForm(f.owner, { run: qwen });
  for (issue of codes) {
    await submit(f.root);
    const control = named(f.root, issue.control);
    assert.equal(control.getAttribute('aria-invalid'), 'true', issue.code);
    const note = find(control.parent, el => el.getAttribute('id') === control.getAttribute('aria-describedby'));
    assert.equal(note.textContent, 'Field refusal');
    const link = find(f.root, el => el.tag === 'a' && el.textContent === 'Field refusal');
    link.dispatchEvent(new Event('click', { cancelable: true }));
    assert.equal(document.activeElement, control);
  }
  change(named(f.root, 'concurrency_qwen_0'), 0, 'input');
  await submit(f.root);
  assert.match(f.root.textContent, /At least one run must be active/);
  change(named(f.root, 'concurrency_qwen_0'), 5, 'input');
  change(named(f.root, 'requests_per_step'), 2, 'input');
  issue = { code: 'invalid_field', key: 'runs[0].workload.requests_per_step' };
  await submit(f.root);
  assert.equal(named(f.root, 'requests_per_step').getAttribute('aria-invalid'), 'true');
  const writes = f.calls.filter(call => call.path === '/calibrations');
  assert.equal(writes.at(-1).body.runs[0].workload.requests_per_step, 2);
  assert.equal(writes.length, codes.length + 1);
  assert.equal(new Set(writes.map(call => call.idempotencyKey)).size, writes.length);
});

test('group validation puts its note immediately after the ready-runs fieldset', async t => {
  const f = fixture(t);
  await calibrationForm(f.owner, { node });
  await submit(f.root);
  const choices = find(f.root, el => el.tag === 'legend' && el.textContent === 'Ready runs').parentElement;
  const siblings = choices.parentElement.childNodes;
  const note = siblings[siblings.indexOf(choices) + 1];
  assert.equal(note.getAttribute('id'), choices.getAttribute('aria-describedby'));
  assert.equal(note.textContent, 'Choose at least one run with a workload.');
});

test('a ready model without an endpoint is resident only', async t => {
  const resident = { ...qwen, id: 'legacy', model: 'org/legacy', endpoint: undefined, target_revision: 'l1' };
  const f = fixture(t, { list: path => path.startsWith('/runs?') ? [qwen, resident] : undefined,
    data: path => path.startsWith('/runs/legacy?') ? resident : path === '/calibrations' ? { job_id: job.job_id, node } : undefined });
  await inspectWorkload(f.owner, 'runs', node, resident.id); await drain();
  assert.ok(!walk(f.root).some(el => el.tag === 'button' && el.textContent === 'Calibrate'));
  await calibrationForm(f.owner.panel.view('New calibration'), { run: qwen });
  change(named(f.root, 'calibration_run_1'), true);
  assert.match(f.root.textContent, /Resident only/);
  await submit(f.root);
  assert.deepEqual(f.calls.find(call => call.path === '/calibrations').body.runs[1], { run_id: resident.id, target_revision: 'l1' });
});

test('manual run refresh preserves a focused draft and refuses known changed revisions', async t => {
  let revision = 'q1';
  const f = fixture(t, { list: path => path.startsWith('/runs?') ? [{ ...qwen, target_revision: revision }] : undefined });
  await calibrationForm(f.owner, { run: qwen });
  await drain();
  const tokens = named(f.root, 'input_tokens');
  tokens.focus(); change(tokens, 177, 'input');
  revision = 'q2';
  await click(f.root, 'Refresh ready runs');
  assert.equal(named(f.root, 'input_tokens'), tokens);
  assert.equal(tokens.value, '177');
  assert.equal(document.activeElement, tokens);
  assert.match(f.root.textContent, /Selection changed/);
  await submit(f.root);
  assert.match(f.root.textContent, /Choose a current ready run with a target revision/);
  assert.ok(!f.calls.some(call => call.path === '/calibrations'));
});

test('opening calibration from a stale run detail requires an explicit reselection', async t => {
  const fresh = { ...qwen, target_revision: 'q2' };
  const f = fixture(t, { list: path => path.startsWith('/runs?') ? [fresh] : undefined,
    data: path => path.startsWith('/runs/qwen?') ? fresh : path === '/calibrations' ? { job_id: job.job_id, node } : undefined });
  await calibrationForm(f.owner, { run: qwen });
  assert.match(f.root.textContent, /The run changed before the form opened/);
  await submit(f.root);
  assert.ok(!f.calls.some(call => call.path === '/calibrations'));
  const choice = named(f.root, 'calibration_run_0');
  change(choice, false); change(choice, true);
  await submit(f.root);
  assert.equal(f.calls.find(call => call.path === '/calibrations').body.runs[0].target_revision, 'q2');
});

test('RFC 9457 calibration refusals link the active job and identify the affected run', async t => {
  const busy = { code: 'calibration_running', job_id: job.job_id, detail: 'A calibration is active. Follow that job with zz_job.' };
  const changed = { code: 'run_changed', run_id: embed.id, detail: 'A run has a different target revision. Read it with zz_runs.' };
  let refusal = changed;
  const api = new API({ fetcher: async path => {
    assert.equal(path, '/zzrouter/v1/calibrations');
    return Response.json({ type: 'about:blank', title: 'Conflict', status: 409, instance: path, request_id: 'calibration-request', ...refusal },
      { status: 409, headers: { 'Content-Type': 'application/problem+json' } });
  } });
  api.state = { csrf: 'fixture' };
  const f = fixture(t, { data: (path, options) => path === '/calibrations' ? api.data(path, options) : undefined });
  await calibrationForm(f.owner, { run: qwen });
  change(named(f.root, 'calibration_run_1'), true);
  await drain();
  await submit(f.root);
  assert.equal(named(f.root, 'calibration_run_1').getAttribute('aria-invalid'), 'true');
  assert.notEqual(named(f.root, 'calibration_run_0').getAttribute('aria-invalid'), 'true');
  refusal = busy;
  await submit(f.root);
  assert.equal(named(f.root, 'calibration_node').getAttribute('aria-invalid'), 'true');
  await click(f.root, 'View calibration job');
  assert.match(f.root.textContent, /Calibration report/);
  assert.ok(f.calls.some(call => call.path === '/jobs/cal%2Ffirst?node=gpu%2F1'));
});

test('missing discovery and offline nodes stay read only, and interrupted submission keeps one operation', async t => {
  let discovery = {};
  const f = fixture(t, { data: path => {
    if (path === '') return discovery;
    if (path === '/calibrations') return Promise.reject(new APIError('Lost response', { uncertain: true }));
  } });
  await calibrationForm(f.owner);
  assert.match(f.root.textContent, /Calibration limits are unavailable/);
  discovery = { calibration: { ...config, default_step_timeout_seconds: undefined } };
  await calibrationForm(f.owner.panel.view('New calibration'), { run: qwen });
  assert.match(f.root.textContent, /Calibration limits are unavailable/);
  discovery = { calibration: config };
  await calibrationForm(f.owner.panel.view('New calibration'), { node: 'offline' });
  assert.match(f.root.textContent, /This node is offline/);
  assert.ok(!f.calls.some(call => call.path === '/runs?node=offline'));
  await calibrationForm(f.owner.panel.view('New calibration'), { run: qwen });
  await submit(f.root); await submit(f.root);
  assert.equal(f.calls.filter(call => call.path === '/calibrations').length, 1);
  assert.match(f.root.textContent, /Operation ID/);
  await click(f.root, 'Inspect calibrations');
  assert.deepEqual(f.routes.at(-1), ['workloads', workloadKey('node', node, ''), 'calibrations']);
});

test('edited grids pad idle runs and honor discovery limits smaller than the release defaults', async t => {
  const limits = { ...config, max_runs: 2, max_steps: 2, max_concurrency: 12, max_requests_per_step: 24, max_step_timeout_seconds: 53, default_step_timeout_seconds: 21 };
  const capped = { ...qwen, max_concurrent_requests: 9 };
  const f = fixture(t, { list: path => path.startsWith('/runs?') ? [capped, embed] : undefined,
    data: path => path.startsWith('/runs/qwen?') ? capped : path === '/calibrations' ? { job_id: job.job_id, node } : undefined });
  await calibrationForm(f.owner, { run: capped, config: limits });
  change(named(f.root, 'calibration_run_1'), true);
  await click(f.root, 'Add step');
  const cell = named(f.root, 'concurrency_qwen_1');
  assert.equal(document.activeElement, cell);
  assert.equal(cell.getAttribute('max'), '9');
  assert.equal(named(f.root, 'concurrency_embed_1').getAttribute('max'), '12');
  assert.equal(named(f.root, 'requests_per_step').getAttribute('max'), '24');
  assert.equal(named(f.root, 'step_timeout_seconds').getAttribute('max'), '53');
  assert.ok(!walk(f.root).some(el => el.tag === 'button' && el.textContent === 'Add step'));
  change(cell, 9, 'input');
  change(named(f.root, 'requests_per_step'), 9, 'input');
  await submit(f.root);
  const write = f.calls.find(call => call.path === '/calibrations');
  assert.deepEqual(write.body.runs.map(run => run.workload.concurrency), [[1, 9], [1, 0]]);
});

test('an acceptance pointing to a different node stays in read-only recovery', async t => {
  const f = fixture(t, { data: path => path === '/calibrations' ? { job_id: job.job_id, node: 'other' } : undefined });
  await calibrationForm(f.owner, { run: qwen });
  await submit(f.root);
  assert.match(f.root.textContent, /did not identify a job on the submitted node/);
  assert.ok(!f.calls.some(call => call.path.startsWith('/jobs/')));
  assert.equal(button(f.root, 'Start calibration').disabled, true);
});

test('Calibrations tab keeps node reads, paging, offline disclosures and an open draft', async t => {
  let choose;
  const f = fixture(t, { request: path => path.includes('/jobs?') ? { data: [job], has_more: path.includes('offset=0') } : undefined });
  f.owner.dispose();
  f.p.handleSelection = handler => { choose = handler; };
  await workloads(f.p, undefined, 'calibrations');
  assert.match(f.root.textContent, /1 offline node skipped/);
  await click(f.root, 'Load older jobs');
  assert.ok(f.calls.some(call => call.path.includes('offset=25&kind=calibration')));
  assert.ok(f.calls.filter(call => call.path.includes('/jobs?')).every(call => call.path.includes('node=gpu%2F1') && call.path.includes('kind=calibration')));
  await choose(workloadKey('node', node, ''), 'create-calibration');
  change(named(f.root, 'calibration_run_0'), true);
  await drain();
  const token = named(f.root, 'input_tokens');
  change(token, 199, 'input'); token.focus();
  await click(f.root, 'Refresh');
  assert.equal(named(f.root, 'input_tokens'), token);
  assert.equal(token.value, '199');
});

test('ready run exposes Calibrate and the model-on-node editor can request a restart', async t => {
  const f = fixture(t, { request: (path, options) => options.method === 'PATCH' ? { runs: { restart_job_id: 'restart/job', stale: [{ id: 'qwen', node }] } } : undefined });
  const shown = f.owner;
  await inspectWorkload(shown, 'runs', node, qwen.id); await drain();
  await click(f.root, 'Calibrate');
  assert.equal(named(f.root, 'calibration_run_0').checked, true);
  await click(f.root, 'Save for this model on this node');
  assert.equal(named(f.root, 'parameter_scope').value, 'node-model');
  assert.equal(named(f.root, 'parameter_node').value, node);
  assert.equal(named(f.root, 'parameter_model').value, qwen.model);
  assert.equal(named(f.root, 'parameters_slots').getAttribute('max'), '32');
  change(named(f.root, 'parameters_slots'), 7, 'input');
  change(named(f.root, 'restart_affected'), true);
  await submit(f.root);
  const patch = f.calls.find(call => call.method === 'PATCH');
  assert.match(patch.path, /\/parameters\?restart=affected$/);
  assert.deepEqual(patch.body.nodes[node].models[qwen.model].endpoints.chat.parameters, { slots: 7 });
  assert.doesNotMatch(f.root.textContent, /Changes take effect on the next start/);
  await click(f.root, 'View restart job');
  assert.deepEqual(f.routes.at(-1), ['workloads', workloadKey('job', 'coord', 'restart/job'), 'jobs']);
});

test('model settings saved without restart explain when they take effect', async t => {
  const f = fixture(t, { request: (path, options) => options.method === 'PATCH' ? {} : undefined });
  await inspectWorkload(f.owner, 'runs', node, qwen.id);
  await click(f.root, 'Save for this model on this node');
  change(named(f.root, 'parameters_slots'), 7, 'input');
  await submit(f.root);
  assert.match(f.calls.find(call => call.method === 'PATCH').path, /\/parameters$/);
  assert.match(f.root.textContent, /Changes take effect on the next start\./);
});

test('opening an empty run settings editor preserves the ordinary preview and start bodies', async t => {
  const f = fixture(t, { data: path => path === '/runs/preview' ? {} : path === '/runs' ? { id: qwen.id } : undefined });
  await startRun(f.owner, nodes, node, { model: qwen.model, provider: qwen.provider });
  const settings = find(f.root, el => el.tag === 'summary' && el.textContent === 'Settings for this run').parentElement;
  settings.open = true; settings.dispatchEvent(new Event('toggle')); await drain();
  assert.ok(named(f.root, 'parameters_slots'));
  await click(f.root, 'Preview start'); await submit(f.root);
  const target = { node, provider: qwen.provider, model: qwen.model, endpoint: 'chat' };
  assert.deepEqual(f.calls.find(call => call.path === '/runs/preview').body, target);
  assert.deepEqual(f.calls.find(call => call.path === '/runs').body, { ...target, auto_deploy: false });
});

test('per-run settings use the request tier, require preview, and offer an explicit replacement after refusal', async t => {
  const f = fixture(t, { data: (path, options) => {
    if (path === '/runs/preview') return { assessment: { status: 'fit' } };
    if (path === '/runs') {
      if (!options.body.replace) throw new APIError('Different request tier', { code: 'run_settings_differ', status: 409, run_id: qwen.id });
      return { id: qwen.id, status: 'starting' };
    }
  } });
  await inspectWorkload(f.owner, 'runs', node, qwen.id); await drain();
  await click(f.root, 'Try on this run only');
  assert.equal(named(f.root, 'parameters_slots').value, '3');
  assert.equal(named(f.root, 'environment_SLOT_DEVICE').value, '');
  await click(f.root, 'Reveal');
  assert.equal(named(f.root, 'environment_SLOT_DEVICE').value, 'synthetic-private');
  change(named(f.root, 'parameters_slots'), 9, 'input');
  change(named(f.root, 'environment_SLOT_DEVICE'), 'changed-device', 'input');
  await submit(f.root);
  assert.ok(!f.calls.some(call => call.path === '/runs'));
  await click(f.root, 'Preview start'); await submit(f.root);
  assert.match(f.root.textContent, /Different request tier/);
  await click(f.root, 'Choose replacement');
  assert.equal(named(f.root, 'run_replace').checked, true);
  assert.equal(document.activeElement, button(f.root, 'Preview start'));
  await submit(f.root);
  await click(f.root, 'Preview start'); await submit(f.root);
  const writes = f.calls.filter(call => call.path === '/runs');
  assert.equal(writes.length, 2);
  assert.notEqual(writes[0].idempotencyKey, writes[1].idempotencyKey);
  assert.deepEqual(writes[1].body, { node, provider: qwen.provider, model: qwen.model, endpoint: 'chat',
    parameters: { slots: 9 }, environment: { SLOT_DEVICE: 'changed-device' }, replace: true, auto_deploy: false });
  assert.ok(f.calls.filter(call => call.path === '/runs/preview').every(call => !Object.hasOwn(call.body, 'replace')));
  assert.doesNotMatch(f.root.textContent, /synthetic-private|changed-device/);
});

for (const replacementID of ['replacement', qwen.id]) test(`the ready replacement restores the calibration draft with ${replacementID === qwen.id ? 'the same ID and a new revision' : 'a new ID'}`, async t => {
  let started = false, ready = false;
  const replacement = { ...qwen, id: replacementID, target_revision: 'q2' };
  const f = fixture(t, {
    list: path => path.startsWith('/runs?') ? [ready ? replacement : qwen, embed] : undefined,
    data: (path, options) => {
      if (path === '/runs/preview') return {};
      if (path === '/runs' && options.method === 'POST') { started = true; return { id: replacementID }; }
      if (started && path.startsWith(`/runs/${replacementID}?`)) return { ...replacement, status: ready ? 'running' : 'starting' };
      if (path === '/calibrations') return { job_id: job.job_id, node };
    },
  });
  await calibrationForm(f.owner, { run: qwen });
  change(named(f.root, 'calibration_run_1'), true); await drain();
  change(named(f.root, 'input_tokens'), 321, 'input');
  change(named(f.root, 'concurrency_qwen_0'), 2, 'input');
  change(named(f.root, 'step_timeout_seconds'), 450, 'input');
  await click(f.root, 'Try on this run only');
  change(named(f.root, 'parameters_slots'), 4, 'input');
  change(named(f.root, 'run_replace'), true);
  await click(f.root, 'Preview start'); await submit(f.root);
  assert.match(f.root.textContent, /Run status: starting/);
  assert.doesNotMatch(f.root.textContent, /Selected replacement run/);
  ready = true;
  await click(f.root, 'Inspect current state');
  assert.ok(f.root.textContent.includes(`Selected replacement run ${replacementID}.`));
  await submit(f.root);
  assert.deepEqual(f.calls.find(call => call.path === '/calibrations').body, { node, step_timeout_seconds: 450, runs: [
    { run_id: replacementID, target_revision: 'q2', workload: { input_tokens: 321, output_tokens: 64, concurrency: [2] } },
    { run_id: embed.id, target_revision: 'e1', workload: { input_tokens: 512, concurrency: [1] } },
  ] });
});

test('a ready replacement without a revision stays in the start result', async t => {
  const f = fixture(t, { data: path => {
    if (path === '/runs/preview') return {};
    if (path === '/runs') return { id: 'replacement' };
    if (path.startsWith('/runs/replacement?')) return { ...qwen, id: 'replacement', target_revision: undefined };
  } });
  await calibrationForm(f.owner, { run: qwen });
  await drain();
  await click(f.root, 'Try on this run only');
  change(named(f.root, 'run_replace'), true);
  await click(f.root, 'Preview start'); await submit(f.root);
  assert.match(f.root.textContent, /The replacement run did not report a revision/);
  assert.doesNotMatch(f.root.textContent, /Selected replacement run/);
});

test('a completed download uses preview and waits for exact run readiness before opening calibration', async t => {
  let status = 'starting';
  const deployment = { id: 'download', model: qwen.model, status: 'completed', nodes: [{ node, status: 'completed' }] };
  const f = fixture(t, { data: path => {
    if (path === '/deployments/download') return deployment;
    if (path === '/runs/preview') return {};
    if (path === '/runs') return { id: qwen.id, job_id: 'run-job' };
    if (path.startsWith('/runs/qwen?')) return { ...qwen, status };
  } });
  await inspectDeployment(f.owner, deployment.id);
  await click(f.root, 'Start and calibrate');
  await click(f.root, 'Preview start'); await submit(f.root);
  assert.match(f.root.textContent, /Run status: starting/);
  assert.ok(!f.calls.some(call => call.path.startsWith('/runs?')));
  status = 'running';
  await click(f.root, 'Inspect current state');
  assert.match(f.root.textContent, /New calibration/);
  assert.equal(named(f.root, 'calibration_run_0').checked, true);
});

test('live calibration uses one paused job reader and existing node-scoped cancellation', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let current = { ...job, seq: 1, phase: 'running', step: 'measuring', meta: { step: 0, steps: 3,
    requests_completed: { qwen: 0, embed: 7 }, requests_total: { qwen: 10, embed: null },
    sample: { at: '2026-11-21T19:54:00Z', pools: [{ id: 'GPU-1', kind: 'gpu', total_mib: 1000, free_mib: 699, used_mib: null, utilization_pct: 0, power_w: 88 },
      { id: 'host', kind: 'host', available_mib: 400, cpu_pct: 6 }] } } };
  const f = fixture(t, { data: path => path.startsWith('/jobs/') ? current : undefined,
    request: (path, options) => {
      if (options.method === 'DELETE') { current = { ...job, phase: 'failed', meta: { report: { ...report, outcome: 'cancelled' } } }; return { accepted: true }; }
    } });
  await inspectWorkload(f.owner, 'jobs', node, job.job_id);
  assert.match(f.root.textContent, /Measuring.*Step 1 of 3.*qwen: 0 \/ 10.*embed: 7 \/ Unknown/);
  assert.match(f.root.textContent, /Used \/ totalUnknown \/ 1,000 MiBUtilization0%Power88 W/);
  assert.match(f.root.textContent, /Host memoryAvailable400 MiB.*CPU6%/);
  assert.doesNotMatch(f.root.textContent, /301 MiB/);
  await click(f.root, 'Pause updates');
  t.mock.timers.tick(5000); await drain();
  assert.equal(f.calls.length, 1);
  await click(f.root, 'Refresh');
  assert.equal(f.calls.length, 2);
  await click(f.root, 'Resume updates');
  t.mock.timers.tick(5000); await drain();
  assert.equal(f.calls.length, 3);
  await click(f.root, 'Cancel job');
  assert.match(f.root.textContent, /cancelled/);
  assert.equal(f.calls.filter(call => call.method === 'DELETE').length, 1);
  assert.ok(f.calls.every(call => call.path.includes('/jobs/cal%2Ffirst?node=gpu%2F1')));
  assert.equal(button(f.root, 'Cancel job').disabled, true);
});

for (const [outcome, code, tool, label, feedback] of [
  ['verified'],
  ['partial', 'target_not_met', 'zz_params_set', 'Change model settings', /engine settings/],
  ['failed', 'requests_failed', 'zz_logs', 'View runtime logs', /Runtime logs/],
  ['inconclusive', 'run_changed', 'zz_runs', 'View run details', /Run details/],
  ['inconclusive', 'usage_unavailable', 'zz_providers', 'View provider details'],
  ['inconclusive', 'step_timeout', 'zz_calibrate', 'Edit workload'],
  ['cancelled', 'cancelled'],
]) test(`report renders ${outcome}${code ? ' with ' + code : ''} and its declared action`, async t => {
  const f = fixture(t, { data: path => path === '/calibrations' ? { job_id: job.job_id, node } : undefined });
  const reason = code ? { code, message: 'Synthetic reason', next_action: { tool: tool || null } } : null;
  f.owner.root.append(calibrationJob(f.owner, { ...job, meta: { report: { ...report, outcome, reason } } }));
  assert.equal(find(f.root, el => el.className?.startsWith('badge')).textContent, outcome);
  if (reason) assert.match(f.root.textContent, /Synthetic reason/);
  if (!tool) {
    const actions = walk(f.root).filter(el => el.tag === 'button').map(el => el.textContent);
    assert.deepEqual(actions, ['×', 'Run again', 'Compare with…', 'Save as model settings on this node']);
    return;
  }
  await click(f.root, label);
  if (feedback) assert.match(f.root.textContent, feedback);
  if (tool === 'zz_providers') assert.deepEqual(f.routes.at(-1), ['providers', 'local/engine', undefined, node]);
  if (tool === 'zz_logs') assert.ok(f.calls.some(call => call.path === '/runs/qwen/logs?node=gpu%2F1&lines=250'));
  if (tool === 'zz_runs') assert.ok(f.calls.some(call => call.path === '/runs/qwen?node=gpu%2F1'));
  if (tool === 'zz_calibrate') {
    await submit(f.root);
    const write = f.calls.find(call => call.path === '/calibrations');
    assert.deepEqual(write.body, { ...report.request, runs: [{ ...report.request.runs[0], target_revision: qwen.target_revision }] });
  }
});

test('report actions use the run whose step carries the reason', async t => {
  const f = fixture(t);
  const reason = { code: 'run_changed', message: 'Run changed', next_action: { tool: 'zz_runs' } };
  const steps = [{ ...report.steps[0], runs: [...report.steps[0].runs, { run_id: embed.id, outcome: 'inconclusive', reason }] }];
  f.owner.root.append(calibrationJob(f.owner, { ...job, meta: { report: { ...report, outcome: 'inconclusive', reason, steps } } }));
  await click(f.root, 'View run details');
  assert.ok(f.calls.some(call => call.path === '/runs/embed?node=gpu%2F1'));
  assert.ok(!f.calls.some(call => call.path === '/runs/qwen?node=gpu%2F1'));
});

test('live view renders keyed counts, sample time and unified memory when only one pool is present', t => {
  const f = fixture(t);
  const at = '2026-11-21T19:54:00Z';
  const view = calibrationJob(f.owner, { ...job, phase: 'running', step: 'measuring', meta: { step: 0, steps: 1,
    requests_completed: { qwen: 0 }, requests_total: { qwen: null, embed: 11 },
    sample: { at, pools: [{ id: 'unified', kind: 'unified', available_mib: null, swap_used_mib: 0 }] } } });
  assert.match(view.textContent, /Measuring.*Step 1 of 1.*qwen: 0 \/ Unknown.*embed: Unknown \/ 11/);
  assert.ok(view.textContent.includes(`Sample time${new Date(at).toLocaleString()}`));
  assert.match(view.textContent, /Unified memoryAvailableUnknownSwap used0 MiB/);
  assert.doesNotMatch(view.textContent, /ui: .*requests completed/);
  assert.doesNotMatch(view.textContent, /GPU|CPU/);
});

test('report distinguishes shared GPU, unified pools and unknowns, and shows a safe override diff', async t => {
  const f = fixture(t);
  const unified = { id: 'unified', kind: 'unified', total_mib: 2048 };
  f.owner.root.append(calibrationJob(f.owner, job));
  assert.match(f.root.textContent, /Ownershipshared.*Other run sidecar · PID 42 · Unknown/);
  assert.match(f.root.textContent, /Peak used \/ totalUnknown \/ 1,000 MiBMinimum free499 MiB/);
  assert.doesNotMatch(f.root.textContent, /501 MiB/);
  assert.match(f.root.textContent, /Step 1 · Total\s*verified27/);
  assert.match(f.root.textContent, /queued3425/);
  assert.match(f.root.textContent, /Unknown \/ 340.*GPU-1: 0 MiB92%0/);
  const chart = find(f.root, el => el.tag === 'svg');
  assert.equal(chart.namespaceURI, 'http://www.w3.org/2000/svg');
  assert.match(chart.textContent, /Step 1: 25 per user, 34 total/);
  const onlyUnified = { ...report, resources: [unified], steps: [{ ...report.steps[0], resources: [{ id: 'unified', minimum_available_mib: null, peak_swap_used_mib: 0 }] }] };
  const unifiedView = calibrationJob(f.owner, { ...job, meta: { report: onlyUnified } });
  assert.match(unifiedView.textContent, /Unified memory.*Per-process attribution is unavailable.*Minimum availableUnknownPeak swap used0 MiB/);
  assert.doesNotMatch(unifiedView.textContent, /Host memory/);
  await click(f.root, 'Save as model settings on this node');
  assert.match(f.root.textContent, /Settings diffslots2 → 3/);
  assert.doesNotMatch(f.root.textContent, /synthetic-private/);
  assert.ok(!f.calls.some(call => call.method === 'PATCH'));
});

test('saving report settings excludes and identifies redacted parameter keys', async t => {
  const f = fixture(t, { request: (path, options) => {
    if (path.endsWith('/schema')) return { ...schema, parameters: { ...schema.parameters, 'engine-note': { type: 'string' }, label: { type: 'string' } } };
    if (options.method === 'PATCH') return {};
  } });
  const tested = { ...report.runs[0], overridden: ['slots', 'engine-note', 'label'], redacted_parameters: ['engine-note'],
    resolved_parameters: { slots: '3', 'engine-note': 'before [REDACTED] after', label: '[REDACTED]' } };
  f.owner.root.append(calibrationJob(f.owner, { ...job, meta: { report: { ...report, runs: [tested] } } }));
  assert.match(f.root.textContent, /Settings prefill omitted redacted values for engine-note/);
  await click(f.root, 'Save as model settings on this node');
  assert.equal(named(f.root, 'parameters_engine-note').value, '');
  assert.equal(named(f.root, 'parameters_label').value, '[REDACTED]');
  await submit(f.root);
  const patch = f.calls.find(call => call.method === 'PATCH');
  assert.deepEqual(patch.body.nodes[node].models[qwen.model].endpoints.chat.parameters, { slots: 3, label: '[REDACTED]' });
});

test('run again carries workload values and deliberately rereads the run revision', async t => {
  const f = fixture(t, { data: path => path === '/calibrations' ? { job_id: job.job_id, node } : undefined });
  f.owner.root.append(calibrationJob(f.owner, job));
  await click(f.root, 'Run again');
  assert.equal(named(f.root, 'step_timeout_seconds').value, '411');
  assert.equal(named(f.root, 'requests_per_step').value, '11');
  await submit(f.root);
  const write = f.calls.find(call => call.path === '/calibrations');
  assert.deepEqual(write.body.runs[0], { ...report.request.runs[0], target_revision: 'q1' });
});

test('compare uses the owning node, pages jobs, displays parameter differences and handles expired reports', async t => {
  const second = { ...job, job_id: 'cal/second' };
  let expired = false;
  const f = fixture(t, { request: path => path.includes('/jobs?') ? { data: path.includes('offset=0') ? [job] : [second], has_more: path.includes('offset=0') } : undefined,
    data: path => {
      if (!path.includes('cal%2Fsecond')) return;
      if (expired) throw new APIError('Report expired', { status: 404 });
      return { ...second, meta: { report: { ...report, runs: [{ ...report.runs[0], run_id: 'new-run', resolved_parameters: { slots: '4' } }] } } };
    } });
  f.owner.root.append(calibrationJob(f.owner, job));
  await click(f.root, 'Compare with…');
  await click(f.root, 'Load older calibrations');
  const picker = named(f.root, 'comparison_job');
  assert.deepEqual(picker.children.map(option => option.value), ['', second.job_id]);
  change(picker, second.job_id);
  await drain();
  assert.deepEqual(walk(f.root).filter(el => el.tag === 'mark').map(el => el.textContent), ['3 (different)', '4 (different)']);
  assert.equal(walk(f.root).filter(el => el.tag === 'table').length, 2);
  assert.ok(f.calls.every(call => call.path.includes('node=gpu%2F1')));
  assert.ok(f.calls.filter(call => call.path.includes('limit=25')).every(call => call.path.includes('kind=calibration')));
  expired = true;
  change(picker, second.job_id); await drain();
  assert.match(f.root.textContent, /Report expired/);
  assert.equal(walk(f.root).filter(el => el.tag === 'table').length, 0);
});
