import test from 'node:test';
import assert from 'node:assert/strict';
import { API, APIError } from '../api.mjs';
import { memoryRows, memorySummary, observeActivity, activityLabel, activityText } from '../observations.mjs';
import { readWorkloadPages, newerJob, jobPercent, jobProgress, launchTargets, workloadKey } from '../workload-data.mjs';
import { startRun } from '../run-form.mjs';
import { followStart, inspectWorkload, runDetails } from '../workload-details.mjs';
import { workloads } from '../workloads.mjs';
import { problem } from '../dom.mjs';
import { liveReads } from '../live-reads.mjs';
import { installDOM, named, button, find, drain, walk } from './dom-fixture.mjs';

const GIB = 1073741824;
const models = [{ name: 'local/model', node: 'worker/a', assigned_app: 'engine' }];
const providers = [{ key: 'engine', name: 'Engine', node: 'worker/a', enabled: true, run_lifecycle: 'process' }];
function fixture(t) {
  const root = installDOM(t), controller = new AbortController(), calls = [], routes = [];
  t.after(() => controller.abort());
  const p = { root, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, state: { node_name: 'coord' }, syncActivity() {}, close() {}, go: (...args) => routes.push(args),
    api: { canLeave: () => true, canChange: () => true, list: async path => path.startsWith('/models') ? models : path.startsWith('/providers') ? providers : [{ name: 'worker/a' }],
      data: async (path, options = {}) => { calls.push({ path, ...options }); return {}; } } };
  return { root, controller, calls, routes, p };
}
const click = async (root, name) => { button(root, name).dispatchEvent(new Event('click')); await drain(); };

test('measured zero, missing usage, partial GPUs and unified memory stay distinct', () => {
  const node = { health_status: 'healthy', memory: { total_gb: 64, available_gb: 32, sample: { total_bytes: 64*GIB, used_bytes: 17*GIB } },
    gpu: { gpus: [{ index: 0, sample: { total_bytes: 24*GIB, used_bytes: 0 } }, { index: 1, memory_total_gb: 24 }, { index: 2, name: 'Integrated graphics' }] } };
  assert.match(memorySummary(node), /RAM  17 \/ 64/);
  assert.match(memorySummary(node), /GPU 0  0 \/ 24/);
  assert.match(memorySummary(node), /GPU 1  Unavailable/);
  assert.doesNotMatch(memorySummary(node), /GPU 2/);
  assert.equal(memoryRows(node).length, 4, 'details still disclose the unmeasured GPU');
  node.gpu.unified_memory = true;
  assert.equal(memoryRows(node).length, 1);
  assert.equal(memoryRows(node)[0].label, 'Unified RAM');
  node.health_status = 'down'; assert.ok(memoryRows(node)[0].stale);
});

test('activity expires from request start, without another successful refresh', t => {
  const { root, p } = fixture(t);
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let now = 100;
  t.mock.method(performance, 'now', () => now++);
  const activity = { state: 'active', valid_for_seconds: 0.001, stale: false };
  observeActivity(activity, -10);
  assert.equal(activityLabel(activity), 'Activity unavailable');
  observeActivity(activity, now);
  const label = activityText(p, activity); root.append(label);
  observeActivity(activity, -10); // deadline passes while no poll is possible
  t.mock.timers.tick(10);
  assert.equal(label.textContent, 'Activity unavailable');
  assert.equal(activityLabel({ state: 'active', engine_busy: true }), 'Activity unavailable');
});

test('workload reads bound concurrency and preserve exact node identity and partial failure', async t => {
  const { p } = fixture(t); let active = 0, maximum = 0;
  p.api.request = async path => {
    maximum = Math.max(maximum, ++active); await drain(); active--;
    const node = new URL(path, 'https://example.test').searchParams.get('node');
    if (node === 'down') throw new Error('unreachable');
    return { data: [{ id: 'same', node: 'untrusted alias' }] };
  };
  const results = await readWorkloadPages(p, ['worker/a', 'worker/b', 'down', 'other'], 'runs');
  assert.equal(maximum, 3);
  assert.equal(results.filter(row => row.error).length, 1);
  assert.notEqual(workloadKey('run', 'worker/a', 'same'), workloadKey('run', 'worker/b', 'same'));
  assert.equal(results.find(row => row.node === 'worker/a').rows[0].node, 'worker/a');
});

test('job progress and event ordering never invent an omitted percentage', () => {
  assert.equal(jobPercent({}), undefined);
  assert.equal(jobPercent({ percent: 0 }), 0);
  assert.equal(jobPercent({ bytes: { total: 100 } }), undefined);
  assert.equal(jobPercent({ bytes: { done: 0, total: 100 } }), 0);
  const current = { epoch: 'one', seq: 9, at: '2026-11-13T10:00:00Z' };
  assert.equal(newerJob(current, { ...current, seq: 8 }), current);
  assert.equal(newerJob(current, { epoch: 'old', seq: 99, at: '2026-11-12T10:00:00Z' }), current);
});

test('launch choices use API lifecycle and exact catalog assignment', () => {
  const candidates = [...models, { ...models[0], name: 'partial', incomplete: true }, { ...models[0], name: 'cloud', is_cloud: true }, { ...models[0], name: 'other', node: 'other' }];
  assert.equal(launchTargets(candidates, providers, 'worker/a').length, 1);
  assert.equal(launchTargets(candidates, [{ ...providers[0], run_lifecycle: undefined }], 'worker/a').length, 0);
  assert.equal(launchTargets(candidates, [{ ...providers[0], run_lifecycle: 'none' }], 'worker/a').length, 0);
  const service = launchTargets([], [{ ...providers[0], run_scope: 'node' }], 'worker/a')[0];
  assert.equal(service.model, undefined);
});

test('preview, start, completed job and exact run readback form one immutable operation', async t => {
  const f = fixture(t);
  f.p.api.data = async (path, options = {}) => {
    f.calls.push({ path, ...options });
    if (path === '/runs/preview') return { assessment: { status: 'unknown', reason: 'No GPU sample' }, environment: { SECRET: 'hidden-value' }, full_command: 'hidden-command' };
    if (path === '/runs') return { id: 'run/1', job_id: 'job/1', status: 'starting' };
    if (path.startsWith('/jobs/')) return { job_id: 'job/1', phase: 'done', kind: 'launch' };
    if (path.startsWith('/runs/')) return { id: 'run/1', status: 'starting', provider: 'engine', model: 'local/model' };
    assert.fail(path);
  };
  await startRun(f.p, [
    { name: 'offline', health_status: 'down', providers: [{ run_lifecycle: 'process' }] },
    { name: 'cloud-only', health_status: 'healthy', providers: [{ run_lifecycle: 'none' }] },
    { name: 'worker/a', health_status: 'healthy', providers: [{ run_lifecycle: 'process' }] },
  ]);
  const nodes = named(f.root, 'run_node');
  assert.equal(nodes.value, 'worker/a');
  assert.deepEqual(nodes.children.filter(option => option.disabled).map(option => option.value), ['offline', 'cloud-only']);
  named(f.root, 'run_target').value = '0'; named(f.root, 'run_target').dispatchEvent(new Event('change'));
  await click(f.root, 'Preview start');
  assert.doesNotMatch(f.root.textContent, /hidden-value|hidden-command/);
  find(f.root, node => node.tag === 'form').dispatchEvent(new Event('submit', { cancelable: true })); await drain(); await drain();
  const start = f.calls.find(call => call.path === '/runs');
  assert.deepEqual(start.body, { node: 'worker/a', provider: 'engine', model: 'local/model', endpoint: 'chat', auto_deploy: false });
  assert.ok(start.idempotencyKey); assert.equal(start.timeoutMs, 120000);
  assert.ok(f.calls.some(call => call.path === '/runs/run%2F1?node=worker%2Fa'));
  assert.match(f.root.textContent, /Run status: starting/);
  assert.doesNotMatch(f.root.textContent, /This run is reported running/);
  await click(f.root, 'Inspect current state');
  assert.equal(f.calls.filter(call => call.path === '/runs').length, 1);
});

test('uncertain start without identity retains recovery and never requests undefined IDs', async t => {
  const f = fixture(t); f.p.api.list = async path => { f.calls.push({ path }); return []; };
  await followStart(f.p, { node: 'worker/a', provider: 'engine', model: 'local/model' }, 'operation-id', undefined, new APIError('response lost', { uncertain: true }));
  assert.match(f.root.textContent, /operation-id/);
  assert.match(f.root.textContent, /empty list does not prove failure/);
  await click(f.root, 'Inspect current state');
  assert.ok(f.calls.every(call => call.path === '/runs?node=worker%2Fa' && !call.method));
});

test('stop remains exact-node and read-only after transport interruption', async t => {
  const f = fixture(t);
  f.p.api.data = async (path, options = {}) => {
    f.calls.push({ path, ...options });
    if (options.method === 'DELETE') throw new APIError('lost', { uncertain: true, mutation_applied: true });
    return { id: 'same/id', node: 'worker/a', provider: 'engine', model: 'local/model', status: 'running' };
  };
  await inspectWorkload(f.p, 'runs', 'worker/a', 'same/id');
  await click(f.root, 'Stop run'); await click(f.root, 'Confirm stop');
  assert.equal(f.calls.find(call => call.method === 'DELETE').timeoutMs, 60000);
  await click(f.root, 'Inspect current state');
  assert.equal(f.calls.filter(call => call.method === 'DELETE').length, 1);
  assert.ok(f.calls.every(call => call.path === '/runs/same%2Fid?node=worker%2Fa'));
  assert.ok(button(f.root, 'Stop run').disabled);
});

test('shared errors preserve Problem context and render bounded evidence as text', async t => {
  const f = fixture(t); let sent;
  const api = new API({ fetcher: async (path, options) => { sent = options; return { ok: false, status: 503, json: async () => ({ detail: '<script>bad</script>', code: 'no_capacity', node: 'worker', request_id: 'request', mutation_applied: true, run_id: 'run', errors: [{ key: 'memory', hint: 'Free capacity', got: { required: 300, environment: { SECRET: 'never-display' } } }] }) }; } });
  api.state = { csrf: 'fixture' };
  let failure;
  try { await api.data('/runs', { method: 'POST', body: {}, idempotencyKey: 'one' }); } catch (error) { failure = error; }
  assert.equal(sent.headers['Idempotency-Key'], 'one'); assert.equal(failure.run_id, 'run');
  f.root.append(problem(failure));
  assert.match(f.root.textContent, /Free capacity/); assert.match(f.root.textContent, /300/);
  assert.doesNotMatch(f.root.textContent, /never-display/);
  assert.equal(walk(f.root).filter(node => node.tag === 'script').length, 0);
});

test('polling has no overlap, stops on failure and supports explicit retry', async t => {
  const f = fixture(t); t.mock.timers.enable({ apis: ['setTimeout'] });
  let resolve, calls = 0;
  const live = liveReads(f.p, () => { calls++; return new Promise(done => { resolve = done; }); });
  const first = live.refresh(); live.refresh(); assert.equal(calls, 1);
  resolve(false); await first;
  t.mock.timers.tick(10000); assert.equal(calls, 1);
  const retry = live.refresh(); assert.equal(calls, 2); resolve(true); await retry;
  live.toggle(); t.mock.timers.tick(10000); assert.equal(calls, 2);
});

test('Workloads keeps partial failures visible and opens matching side panels', async t => {
  const f = fixture(t); let choose;
  f.p.handleSelection = fn => { choose = fn; };
  f.p.go = async (page, selection, mode) => choose(selection, mode);
  f.p.api.list = async () => [{ name: 'worker/a' }, { name: 'down' }];
  f.p.api.request = async path => {
    if (path.includes('node=down')) throw new Error('Worker unavailable');
    return { data: [{ id: 'run', provider: 'engine', model: 'local/model', status: 'running' }] };
  };
  f.p.api.data = async () => ({ id: 'run', provider: 'engine', model: 'local/model', status: 'running' });
  await workloads(f.p);
  assert.match(f.root.textContent, /Could not refresh runs on down/);
  await click(f.root, 'local/model');
  assert.equal(walk(f.root).filter(node => node.tag === 'dialog').length, 1);
  assert.match(f.root.textContent, /worker\/a/);
});

for (const tab of ['runs', 'jobs']) test(`${tab} skips known offline nodes and resumes reads after recovery`, async t => {
  const f = fixture(t);
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let health = 'down';
  f.p.api.list = async () => [{ name: 'worker/a', health_status: 'healthy' }, { name: 'sleeping-pc', health_status: health }];
  f.p.api.request = async path => {
    f.calls.push({ path });
    if (path.includes('sleeping-pc') && health === 'down') throw new Error('Offline nodes must not be queried');
    return { data: [{ id: 'run', job_id: 'job', model: 'model', meta: { model: 'model' }, provider: 'engine', status: 'running', phase: 'running' }] };
  };
  await workloads(f.p, undefined, tab);
  assert.ok(f.calls.every(call => !call.path.includes('sleeping-pc')));
  assert.match(f.root.textContent, /1 offline node skipped/);
  assert.doesNotMatch(f.root.textContent, /Could not read|Offline nodes must not/);
  health = 'healthy';
  t.mock.timers.tick(5000); await drain();
  assert.ok(f.calls.some(call => call.path.includes('sleeping-pc')));
  assert.doesNotMatch(f.root.textContent, /offline node skipped/);
  assert.equal(walk(f.root).filter(node => node.tag === 'button' && node.textContent === 'model').length, 2);
  health = 'down'; f.calls.length = 0;
  await click(f.root, 'Refresh');
  assert.ok(f.calls.every(call => !call.path.includes('sleeping-pc')));
  assert.equal(walk(f.root).filter(node => node.tag === 'button' && node.textContent === 'model').length, 1, 'offline rows do not look current');
});

test('Jobs pagination is operable, deduplicates events and preserves actual zero speed', async t => {
  const f = fixture(t); f.p.api.list = async () => [{ name: 'worker/a' }];
  f.p.api.request = async path => {
    f.calls.push({ path });
    const older = path.includes('offset=25');
    return { data: [{ job_id: 'same', epoch: 'epoch', seq: older ? 1 : 2, kind: 'download', phase: 'running', at: '2026-11-13T10:00:00Z', percent: older ? 10 : 20, meta: { model: 'weights' } }, ...(older ? [{ job_id: 'older', epoch: 'epoch', seq: 1, kind: 'install', phase: 'done', at: '2026-11-12T10:00:00Z' }] : [])], has_more: !older };
  };
  await workloads(f.p, undefined, 'jobs');
  assert.equal(button(f.root, 'Load older jobs').disabled, false);
  await click(f.root, 'Load older jobs');
  assert.ok(f.calls.some(call => call.path.includes('offset=25')));
  assert.equal(walk(f.root).filter(node => node.tag === 'button' && node.textContent === 'weights').length, 1);
  assert.match(f.root.textContent, /20%/); assert.doesNotMatch(f.root.textContent, /10%/);
  assert.match(jobProgress({ kind: 'download', meta: { transfer_speed: 0 } }).textContent, /Speed: 0 MiB\/s/);
});

test('automatic workload refresh preserves a row focused while the request is pending', async t => {
  const f = fixture(t); t.mock.timers.enable({ apis: ['setTimeout'] });
  let finish, count = 0;
  f.p.api.list = async () => [{ name: 'worker/a' }];
  f.p.api.request = async () => ++count === 1 ? { data: [{ id: 'run', model: 'model', status: 'running' }] } : new Promise(resolve => { finish = resolve; });
  await workloads(f.p);
  const row = button(f.root, 'model');
  t.mock.timers.tick(5000); await drain();
  row.focus(); finish({ data: [{ id: 'run', model: 'new name', status: 'running' }] }); await drain();
  assert.ok(f.root.contains(row)); assert.equal(document.activeElement, row);
  assert.doesNotMatch(f.root.textContent, /new name/);
});

test('a pending node-health refresh preserves newly focused workload controls', async t => {
  const f = fixture(t); t.mock.timers.enable({ apis: ['setTimeout'] });
  let finish, count = 0;
  f.p.api.list = async () => ++count === 1 ? [{ name: 'worker/a' }] : new Promise(resolve => { finish = resolve; });
  f.p.api.request = async () => ({ data: [{ id: 'run', model: 'model', status: 'running' }] });
  await workloads(f.p);
  const selector = named(f.root, 'workload_node');
  const options = [...selector.children];
  t.mock.timers.tick(5000); await drain();
  selector.focus(); finish([{ name: 'replacement' }]); await drain();
  assert.deepEqual([...selector.children], options);
  assert.equal(document.activeElement, selector);
  assert.doesNotMatch(f.root.textContent, /replacement/);
});

test('automatic run detail refresh preserves a focused API gateway link', async t => {
  const f = fixture(t); t.mock.timers.enable({ apis: ['setTimeout'] });
  let finish, count = 0;
  const run = { id: 'run', status: 'running', provider: 'engine', browser: { name: 'Studio', url: 'https://studio.example.test/ui/' } };
  f.p.api.data = async () => ++count === 1 ? run : new Promise(resolve => { finish = resolve; });
  await inspectWorkload(f.p, 'runs', 'worker/a', 'run');
  const link = find(f.root, node => node.tag === 'a');
  assert.equal(link.getAttribute('href'), 'https://studio.example.test/ui/');
  t.mock.timers.tick(5000); await drain(); link.focus(); finish(run); await drain();
  assert.ok(f.root.contains(link)); assert.equal(document.activeElement, link);
  for (const url of ['javascript:alert(1)', 'https://secret:password@studio.example.test/']) assert.equal(walk(runDetails(f.p, { ...run, browser: { name: 'Unsafe', url } })).filter(node => node.tag === 'a').length, 0);
});

test('pausing while an automatic detail read is pending preserves the visible snapshot', async t => {
  const f = fixture(t); t.mock.timers.enable({ apis: ['setTimeout'] });
  let finish, count = 0;
  f.p.api.data = async () => ++count === 1 ? { id: 'run', model: 'original', status: 'running' } : new Promise(resolve => { finish = resolve; });
  await inspectWorkload(f.p, 'runs', 'worker/a', 'run');
  t.mock.timers.tick(5000); await drain();
  await click(f.root, 'Pause updates');
  finish({ id: 'run', model: 'changed', status: 'running' }); await drain();
  assert.match(f.root.textContent, /original/); assert.doesNotMatch(f.root.textContent, /changed/);
  t.mock.timers.tick(10000); assert.equal(count, 2);
});

test('changing node during older-job pagination settles the cancelled read and loads the selected node', async t => {
  const f = fixture(t); let choose, olderStarted = false;
  f.p.handleSelection = fn => { choose = fn; };
  f.p.api.list = async () => [{ name: 'worker/a' }, { name: 'worker/b' }];
  f.p.api.request = async (path, { signal }) => {
    f.calls.push({ path });
    if (path.includes('offset=25')) {
      olderStarted = true;
      return new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(new DOMException('Cancelled', 'AbortError')), { once: true }));
    }
    return { data: [{ job_id: path.includes('worker%2Fb') ? 'from-b' : 'from-a', kind: 'download', phase: 'running', meta: { model: path.includes('worker%2Fb') ? 'model-b' : 'model-a' } }], has_more: true };
  };
  await workloads(f.p, workloadKey('node', 'worker/a', ''), 'jobs');
  await click(f.root, 'Load older jobs'); assert.equal(olderStarted, true);
  await choose(workloadKey('node', 'worker/b', ''), 'jobs');
  assert.equal(named(f.root, 'workload_node').value, 'worker/b');
  assert.match(f.root.textContent, /model-b/); assert.doesNotMatch(f.root.textContent, /model-a/);
});

test('explicit refresh discovers new nodes without broadening a missing node filter', async t => {
  const f = fixture(t); let nodes = [{ name: 'worker/a' }];
  f.p.api.list = async () => nodes;
  f.p.api.request = async path => { f.calls.push({ path }); return { data: [] }; };
  await workloads(f.p, workloadKey('node', 'worker/a', ''));
  nodes = [{ name: 'worker/b' }];
  await click(f.root, 'Refresh');
  const selector = named(f.root, 'workload_node');
  assert.match(selector.textContent, /worker\/b/);
  assert.match(selector.textContent, /worker\/a \(not in current inventory\)/);
  assert.equal(selector.value, 'worker/a');
  assert.ok(f.calls.every(call => call.path.includes('node=worker%2Fa')));
});

for (const nextKind of ['run', 'job']) test(`a newer ${nextKind} selection supersedes a pending tab change without opening a second panel`, async t => {
  const f = fixture(t); let choose, finish, signal;
  f.p.handleSelection = fn => { choose = fn; };
  f.p.api.request = async (path, options) => {
    if (path.includes('/runs?')) return new Promise((resolve, reject) => {
      finish = resolve; signal = options.signal;
      signal.addEventListener('abort', () => reject(new DOMException('Cancelled', 'AbortError')), { once: true });
    });
    return { data: [] };
  };
  f.p.api.data = async path => path.includes('/jobs/') ? { job_id: 'new-job', kind: 'download', phase: 'running', meta: { model: 'new selection' } } : { id: 'new-run', model: 'new selection', status: 'running' };
  await workloads(f.p, undefined, 'jobs');
  const first = choose(workloadKey('run', 'worker/a', 'old-run'), 'runs');
  await drain(); assert.ok(finish);
  const next = choose(workloadKey(nextKind, 'worker/a', `new-${nextKind}`), nextKind === 'job' ? 'jobs' : 'runs');
  if (nextKind === 'run') finish({ data: [] });
  await Promise.all([first, next]);
  assert.equal(signal.aborted, nextKind === 'job');
  assert.equal(walk(f.root).filter(node => node.tag === 'dialog').length, 1);
  assert.match(f.root.textContent, /new selection/);
  assert.doesNotMatch(f.root.textContent, /old-run/);
});
