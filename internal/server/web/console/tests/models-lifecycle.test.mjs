import test from 'node:test';
import assert from 'node:assert/strict';
import { details } from '../details.mjs';
import { models, deleteModel } from '../models.mjs';
import { deploymentForm, discoverModels } from '../model-discovery.mjs';
import { modelKey, modelIdentity, modelQuery, modelInferenceTarget, discoveryKey, discoveryIdentity, nextSearchPage, searchPath, searchSelection, discoveryRows } from '../model-data.mjs';
import { inspectDeployment } from '../deployments.mjs';
import { acceptedInstallOptions, installScript, installReview, installProvider } from '../provider-install.mjs';
import { providerOperation, providerService, remoteOllama, providerVersions } from '../provider-lifecycle.mjs';
import { installDOM, find, named, button, walk, drain } from './dom-fixture.mjs';

const model = { name: 'org/model:Q4', node: 'worker one', assigned_app: 'engine', source_repo: 'huggingface', source_id: 'org/model', size: 123 };
const discovery = { provider: 'huggingface', api_id: 'org/model', display_name: 'Example model' };
const entry = { key: 'engine', name: 'Engine' };
const plan = { plan_id: 'reviewed-1', provider: 'engine-runtime', version: 'v2', action: 'install', steps: [{ step: 0, description: 'Prepare', command: 'prepare' }, { step: 1, description: 'Activate', command: '' }], resolved_install: { selected_components: [] } };
function fixture(t, api = {}) {
  const root = installDOM(t), controller = new AbortController(), calls = [], routes = [];
  t.after(() => controller.abort());
  const p = { root, current: () => !controller.signal.aborted, scope: { signal: controller.signal }, state: { node_name: 'coordinator' }, syncActivity() {},
    go: (...args) => { routes.push(args); }, api: { canLeave: () => true, canChange: () => true,
      data: async (path, options = {}) => { calls.push({ path, ...options }); return {}; },
      request: async (path, options = {}) => { calls.push({ path, ...options }); return {}; },
      list: async (path, options = {}) => { calls.push({ path, ...options }); return []; }, ...api } };
  return { root, p, calls, routes, controller, owner: () => details(p, 'Details', () => {}) };
}
const click = async (root, label) => { button(root, label).dispatchEvent(new Event('click')); await drain(); };
const submit = async root => { find(root, item => item.tag === 'form').dispatchEvent(new Event('submit', { cancelable: true })); await drain(); };

test('model identity retains the exact node/provider/registry/source, and opaque search cursors survive', () => {
  assert.deepEqual(modelIdentity(model), { name: model.name, node: model.node, provider: model.assigned_app, registry: model.source_repo, source_id: model.source_id });
  assert.equal(modelKey(model), 'worker%20one/engine/huggingface/org%2Fmodel%3AQ4/org%2Fmodel');
  assert.equal(modelKey({ ...model, source_id: undefined }), 'worker%20one/engine/huggingface/org%2Fmodel%3AQ4');
  assert.deepEqual(Object.fromEntries(modelQuery(model)), { model: model.name, node: 'worker one', provider: 'engine', registry: 'huggingface', source_id: 'org/model' });
  assert.deepEqual(discoveryIdentity(discoveryKey(discovery)), { provider: discovery.provider, api_id: discovery.api_id });
  const next = nextSearchPage({ pagination: { has_next: true, cursor: { next: 'opaque+/=cursor' } } });
  assert.deepEqual(next, { cursor: 'opaque+/=cursor' });
  assert.equal(new URL('http://fixture' + searchPath({ provider: 'huggingface', ...next })).searchParams.get('cursor'), 'opaque+/=cursor');
  assert.equal(nextSearchPage({ pagination: { has_next: true, offset: 30, cursor: {} } }), undefined);
  assert.deepEqual(nextSearchPage({ pagination: { has_next: true, offset: 30, limit: 30 } }), { offset: 60 });
});

test('declared client filtering and sorting stay out of upstream query parameters', () => {
  const selection = searchSelection({ name: 'cloud', client_filter: true, client_sorts: ['price'] }, { query: 'model', sort: 'price' });
  assert.equal(selection.query, ''); assert.equal(selection.sort, '');
  const results = ['missing', '2', '0'].map(price => ({ discovery: { api_id: 'model', price_prompt: price === 'missing' ? undefined : price } }));
  assert.deepEqual(discoveryRows(results, selection).map(item => item.discovery.price_prompt), ['0', '2', undefined]);
  assert.equal(searchSelection({ name: 'registry' }, { sort: 'newest' }).sort, 'lastModified');
});

test('discovery pagination uses the server cursor and stale provider reads cannot replace newer results', async t => {
  let oldRead;
  const f = fixture(t, { data: async path => {
    f.calls.push({ path });
    if (path === '/search?type=providers') return { results: [{ name: 'a', sort_options: [] }, { name: 'b', sort_options: [] }] };
    const query = new URL('http://fixture' + path).searchParams;
    if (query.get('cursor')) return { results: [{ discovery: { provider: 'a', api_id: 'next-page' } }], pagination: {} };
    if (query.get('provider') === 'b') return new Promise(resolve => { oldRead = resolve; });
    return { results: [{ discovery: { provider: 'a', api_id: 'current' } }], pagination: { has_next: true, cursor: { next: 'opaque=+' } } };
  } });
  await discoverModels(f.p);
  await click(f.root, 'Next');
  assert.equal(new URL('http://fixture' + f.calls.at(-1).path).searchParams.get('cursor'), 'opaque=+');
  const source = named(f.root, 'discovery_provider'); source.value = 'b'; source.dispatchEvent(new Event('change')); await drain();
  source.value = 'a'; source.dispatchEvent(new Event('change')); await drain();
  oldRead({ results: [{ discovery: { provider: 'b', api_id: 'STALE' } }], pagination: {} }); await drain();
  assert.doesNotMatch(f.root.textContent, /STALE/); assert.match(f.root.textContent, /current/);
});

test('available selection opens exact details and preserves the local node when handing off to Chat', async t => {
  const f = fixture(t, { list: async () => [model], data: async () => ({ model_info: { architecture: 'example' } }) });
  await models(f.p, modelKey(model));
  assert.match(f.root.textContent, /org\/model:Q4.*worker one/s);
  await click(f.root, 'Chat'); assert.deepEqual(f.routes.at(-1), ['chat', 'engine/' + model.name, model.node]);
});

test('inference target qualifies the selected provider and only omits the cloud sentinel node', () => {
  assert.deepEqual(modelInferenceTarget({ name: 'shared', assigned_app: 'provider-b', node: 'worker', is_cloud: true }), { model: 'provider-b/shared', node: 'worker' });
  assert.deepEqual(modelInferenceTarget({ name: 'provider-b/shared', assigned_app: 'provider-b', node: 'cloud', is_cloud: true }), { model: 'provider-b/shared', node: undefined });
});

test('same-name worker cloud model Chat and Test both target the selected provider and real node', async t => {
  const first = { ...model, name: 'shared', source_id: 'shared', assigned_app: 'provider-a', node: 'worker-cloud', is_cloud: true }, second = { ...first, assigned_app: 'provider-b' };
  const f = fixture(t, { origin: 'http://fixture', list: async () => [first, second], data: async () => ({}), inferenceFetch: async (url, options) => {
    f.calls.push({ url, ...options });
    return new Response('data: {"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
  } });
  await models(f.p, modelKey(second)); await click(f.root, 'Chat');
  assert.deepEqual(f.routes.at(-1), ['chat', 'provider-b/shared', 'worker-cloud']);
  await click(f.root, 'Test model'); await click(f.root, 'Send test'); await drain();
  assert.equal(JSON.parse(f.calls[0].body).model, 'provider-b/shared');
  assert.equal(new Headers(f.calls[0].headers).get('X-Node'), 'worker-cloud');
});

test('worker cloud model parameters retain the selected node-model scope', async t => {
  const selected = { ...model, assigned_app: 'provider-b', node: 'worker-cloud', is_cloud: true };
  const f = fixture(t, { list: async () => [selected], request: async path => {
    f.calls.push({ path }); return { parameters: {}, environment: {}, available_endpoints: [] };
  } });
  await models(f.p, modelKey(selected));
  await click(f.root, 'Parameters');
  const call = f.calls.find(value => value.path.includes('/parameters?'));
  assert.ok(call);
  const query = new URL(call.path, 'https://router.example').searchParams;
  assert.equal(query.get('scope'), 'node-model');
  assert.equal(query.get('node'), 'worker-cloud');
  assert.equal(query.get('model'), selected.name);
});

test('exact deletion shows HTTP-200 partial errors without reporting full success or replaying', async t => {
  let refreshes = 0;
  const f = fixture(t, { data: async (path, options) => { f.calls.push({ path, ...options }); return { deleted: 0, errors: ['Model is still in use on worker one'] }; } });
  await deleteModel(f.owner(), model, async () => { refreshes++; });
  assert.deepEqual(f.calls[0].body, { models: [modelIdentity(model)] });
  assert.match(f.root.textContent, /0 model copies deleted.*still in use/s);
  await click(f.root, 'Refresh inventory');
  assert.equal(f.calls.length, 1); assert.equal(refreshes, 2);
});

test('same-name model selection and deletion retain the selected physical source', async t => {
  const first = { ...model, name: 'shared', source_id: 'org/a' }, second = { ...first, source_id: 'org/b' };
  let records = [first, second];
  const confirmations = [];
  const f = fixture(t, { list: async () => records, data: async (path, options = {}) => {
    f.calls.push({ path, ...options });
    if (options.method === 'DELETE') { records = [first]; return { deleted: 1 }; }
    return { model_info: { source: second.source_id } };
  } });
  globalThis.confirm = question => { confirmations.push(question); return true; };
  await models(f.p, modelKey(second));
  assert.notEqual(modelKey(first), modelKey(second));
  const rows = walk(f.root).filter(item => item.getAttribute('data-selection'));
  assert.deepEqual(rows.map(item => item.getAttribute('data-selection')), [modelKey(first), modelKey(second)]);
  assert.match(rows[0].textContent, /org\/a/); assert.match(rows[1].textContent, /org\/b/);
  const dialog = find(f.root, item => item.tag === 'dialog');
  assert.match(dialog.textContent, /Sourceorg\/b/); assert.doesNotMatch(dialog.textContent, /org\/a/);
  const query = new URL('http://fixture' + f.calls[0].path).searchParams;
  assert.equal(query.get('model'), 'shared'); assert.equal(query.get('source_id'), 'org/b');
  await click(f.root, 'Chat'); assert.deepEqual(f.routes.at(-1), ['chat', 'engine/shared', second.node]);
  await click(f.root, 'Delete model');
  assert.match(confirmations[0], /source: org\/b/);
  assert.deepEqual(f.calls.find(call => call.method === 'DELETE').body, { models: [modelIdentity(second)] });
  assert.match(dialog.textContent, /Sourceorg\/b.*1 model copies deleted/s);
  assert.deepEqual(records, [first]);
});

test('a source-less old selection never falls back to a same-name physical source', async t => {
  const first = { ...model, name: 'shared', source_id: 'org/a' }, second = { ...first, source_id: 'org/b' };
  const f = fixture(t, { list: async () => [first, second] });
  await models(f.p, modelKey({ ...first, source_id: undefined }));
  assert.match(f.root.textContent, /This exact model is not in the current inventory/);
  assert.equal(walk(f.root).some(item => item.tag === 'button' && item.textContent === 'Delete model'), false);
  assert.equal(f.calls.length, 0);
});

test('cloud model removal uses exact batch registration identity without filesystem deletion wording', async t => {
  const cloud = { name: 'cloud-model', node: 'cloud', assigned_app: 'cloud-provider', source_repo: 'cloud-provider', source_id: 'org/cloud-model', is_cloud: true };
  const confirmations = [];
  const f = fixture(t, { list: async () => [cloud], data: async (path, options = {}) => { f.calls.push({ path, ...options }); return options.method === 'DELETE' ? { deleted: 1 } : {}; } });
  globalThis.confirm = question => { confirmations.push(question); return true; };
  await models(f.p, modelKey(cloud));
  assert.match(f.root.textContent, /Model registration/);
  await click(f.root, 'Remove cloud model');
  assert.equal(f.calls.find(call => call.method === 'DELETE').path, '/models');
  assert.deepEqual(f.calls.find(call => call.method === 'DELETE').body, { models: [modelIdentity(cloud)] });
  assert.match(confirmations[0], /Remove the registration.*cloud-provider.*source: org\/cloud-model/s);
  assert.doesNotMatch(confirmations[0], /files|copy|storage/i);
  assert.match(f.root.textContent, /1 model registrations removed/);
  assert.doesNotMatch(f.root.textContent, /stored files|model copies deleted/);
});

test('late available model details cannot replace another panel view', async t => {
  let resolve; const waiting = new Promise(done => { resolve = done; });
  const f = fixture(t, { list: async () => [model], data: () => waiting });
  const pending = models(f.p, modelKey(model)); await drain();
  await click(f.root, 'Test model');
  resolve({ model_info: { stale: 'OLD PANEL' } }); await pending;
  assert.doesNotMatch(f.root.textContent, /OLD PANEL/); assert.match(f.root.textContent, /Send test/);
});

function downloadFixture(t, mutation) {
  const f = fixture(t, {
    list: async path => path.startsWith('/providers/catalog') ? [{ key: 'engine', name: 'Engine', kind: 'on-demand' }] : [{ name: 'worker one', health_status: 'healthy' }, { name: 'unknown', health_status: 'down' }],
    data: async (path, options = {}) => {
      f.calls.push({ path, ...options });
      if (options.method === 'POST') return mutation(path, options);
      if (path.startsWith('/registries/')) return { variants: [{ file: 'weights-Q4-00001-of-00002.gguf', quantization: 'Q4', shards: 2, size_human: '2 GiB' }], features: [{ feature: 'vision', file: 'projector.gguf', size_human: '1 GiB' }] };
      if (path.startsWith('/deployments/')) return { id: 'download-1', model: discovery.api_id, status: 'completed', nodes: [{ node: 'unknown', status: 'completed' }] };
      return {};
    },
    request: async path => ({ data: [], metadata: { assessments: [{ node: 'worker one', provider: 'engine', status: 'no_fit', reason: 'insufficient memory' }] } }),
  });
  return f;
}
async function chooseDownload(f) {
  await deploymentForm(f.owner(), discovery, () => {});
  const provider = named(f.root, 'download_provider'); provider.value = 'engine'; provider.dispatchEvent(new Event('change')); await drain();
  named(f.root, 'download_file').value = 'weights-Q4-00001-of-00002.gguf';
  named(f.root, 'node_unknown').checked = true;
  named(f.root, 'feature_vision').checked = true;
  await click(f.root, 'Review download');
}

test('download preserves explicit unknown-capacity node, grouped variant and optional feature selection', async t => {
  const f = downloadFixture(t, async () => ({ id: 'download-1', model: discovery.api_id, status: 'downloading', nodes: [{ node: 'unknown', status: 'downloading' }] }));
  await chooseDownload(f);
  assert.match(f.root.textContent, /unknown.*capacity unknown/s);
  assert.match(f.root.textContent, /2 shards/);
  await submit(f.root);
  const write = f.calls.find(call => call.method === 'POST');
  assert.deepEqual(write.body, { model: 'org/model', registry: 'huggingface', provider: 'engine', nodes: ['unknown'], file: 'weights-Q4-00001-of-00002.gguf', features: ['vision'] });
  assert.match(f.root.textContent, /download-1/); assert.match(f.root.textContent, /completed/);
});

test('an interrupted deployment submission leaves only inspection, never a second submit', async t => {
  const f = downloadFixture(t, async () => { throw Object.assign(new Error('Outcome unknown'), { uncertain: true }); });
  await chooseDownload(f); await submit(f.root);
  assert.match(f.root.textContent, /Outcome unknown.*Do not resubmit automatically/s);
  assert.equal(walk(f.root).some(item => item.tag === 'form'), false);
  await click(f.root, 'Inspect Downloads');
  assert.equal(f.calls.filter(call => call.method === 'POST').length, 1);
  assert.deepEqual(f.routes.at(-1), ['workloads', undefined, 'downloads']);
});

test('node cancellation uses exact deployment/node and only refreshes after an uncertain response', async t => {
  const record = { id: 'a/b', model: 'model', status: 'downloading', nodes: [{ node: 'worker one', status: 'downloading', progress: 10 }] };
  const f = fixture(t, { data: async path => { f.calls.push({ path }); return record; }, request: async (path, options) => { f.calls.push({ path, ...options }); throw Object.assign(new Error('cancel interrupted'), { uncertain: true }); } });
  await inspectDeployment(f.owner(), record.id, record);
  await click(f.root, 'Cancel on this node');
  assert.equal(f.calls.find(call => call.method === 'DELETE').path, '/zzrouter/v1/deployments/a%2Fb/nodes/worker%20one');
  assert.match(f.root.textContent, /cancel interrupted/);
  await click(f.root, 'Refresh');
  assert.equal(f.calls.filter(call => call.method === 'DELETE').length, 1);
});

test('accepted install options retain plan binding and explicit empty component selection', () => {
  assert.deepEqual(acceptedInstallOptions(plan, { force: true }), { expected_plan_id: 'reviewed-1', version: 'v2', runtime: 'engine-runtime', action: 'install', force: true, components: [] });
  assert.equal(installScript(plan), null);
  assert.equal(installScript({ platform: { os: 'linux' }, steps: [{ command: 'first' }, { command: "second 'quoted'" }] }), "#!/bin/sh\nset -e\n\nsh -c 'first'\n\nsh -c 'second '\\''quoted'\\'''\n");
});

test('command scripts refuse missing platform and managed execution context without disclosing private inputs', async t => {
  const command = { command: 'install component' };
  for (const context of [{ working_dir: '/managed/strata' }, { env: { TOKEN: 'private-value' } }, { stdin: 'private-stdin' }, { run_as: 'service-user' }, { service_action: 'restart' }, { timeout: '5m' }, { optional: true }, { verify: { type: 'runtime_checks' } }]) {
    assert.equal(installScript({ platform: { os: 'linux' }, steps: [{ ...command, ...context }] }), null);
  }
  assert.equal(installScript({ steps: [command] }), null);
  assert.equal(installScript({ platform: { os: 'windows' }, steps: [command] }), null);
  const f = fixture(t);
  await installReview(f.owner(), entry, model.node, { ...plan, platform: { os: 'linux' }, steps: [{ step: 0, ...command, working_dir: '/managed/strata', env: { TOKEN: 'private-value' }, stdin: 'private-stdin' }] }, {}, () => {});
  assert.match(f.root.textContent, /Managed execution is required/);
  assert.match(f.root.textContent, /Working directory\/managed\/strata/);
  assert.doesNotMatch(f.root.textContent, /Reported command script|Complete command script|private-value|private-stdin/);
});

test('install form uses server-declared runtimes and optional components, then reviews without installing', async t => {
  const f = fixture(t, { request: async () => ({ diagnostics: { runtimes: { engine: {}, 'engine-runtime': {} } } }), data: async (path, options = {}) => {
    f.calls.push({ path, ...options });
    if (path.includes('/environment')) return { desired_install: { recipe: { components: { vision: { description: 'Vision library' } } } }, installed_manifest: { selected_components: ['vision'] } };
    if (path.includes('/preflight')) return { all_ok: false, results: [{ check: 'GPU', passed: false, message: 'Missing driver', hint: 'Install host driver' }] };
    return plan;
  } });
  await installProvider(f.owner(), entry, model.node, () => {});
  assert.match(named(f.root, 'install_runtime').textContent, /engine-runtime/);
  await click(f.root, 'Check prerequisites'); assert.match(f.root.textContent, /Missing driver.*Install host driver/s);
  const runtime = named(f.root, 'install_runtime'); runtime.value = 'engine-runtime'; runtime.dispatchEvent(new Event('change')); await drain();
  const component = named(f.root, 'component_vision'); component.checked = false; component.dispatchEvent(new Event('change'));
  named(f.root, 'install_version').value = 'v2'; named(f.root, 'install_force').checked = true;
  await submit(f.root);
  const preview = f.calls.find(call => call.path.includes('/install/plan'));
  assert.deepEqual(preview.body, { version: 'v2', runtime: 'engine-runtime', force: true, components: [] });
  assert.equal(preview.effectful, false); assert.equal(named(f.root, 'copy-value').value, 'reviewed-1');
  assert.equal(f.calls.some(call => call.method && !call.effectful && !call.path.includes('/plan') && !call.path.includes('/preflight')), false);
});

test('install verification uses distinct endpoint contracts and preserves unverifiable evidence', async t => {
  const f = fixture(t, { data: async (path, options) => { f.calls.push({ path, ...options }); return path.includes('verify-step') ? { step: 0, passed: true, unverifiable: true, message: 'No retained evidence' } : { all_ok: false, steps: [{ step: 0, passed: false, message: 'missing' }] }; } });
  await installReview(f.owner(), entry, model.node, plan, { force: true }, () => {});
  await click(f.root, 'Verify installation');
  assert.deepEqual(f.calls[0].body, { runtime: 'engine-runtime', version: 'v2' });
  await click(f.root, 'Verify step');
  assert.deepEqual(f.calls[1].body, { ...acceptedInstallOptions(plan, { force: true }), step: 0 });
  assert.match(f.root.textContent, /Not verifiable.*No retained evidence/s);
});

test('step execution retains accepted node/job and cannot return to plan until a terminal observation', async t => {
  let terminal = false;
  const f = fixture(t, { data: async (path, options = {}) => {
    f.calls.push({ path, ...options });
    return options.method ? { accepted: true, node: 'accepted node', job_id: 'job/a' } : { phase: terminal ? 'done' : 'running', job_id: 'job/a', epoch: 'same', seq: terminal ? 2 : 1 };
  } });
  await installReview(f.owner(), entry, model.node, plan, { force: true }, () => {});
  await click(f.root, 'Run this step');
  assert.deepEqual(f.calls[0].body, { ...acceptedInstallOptions(plan, { force: true }), step: 0 });
  assert.equal(f.calls[1].path, '/jobs/job%2Fa?node=accepted%20node');
  assert.equal(button(f.root, 'Return to reviewed plan').disabled, true);
  terminal = true; await click(f.root, 'Refresh job');
  assert.equal(button(f.root, 'Return to reviewed plan').disabled, false);
  assert.match(f.root.textContent, /Inspect current provider state to confirm readiness/);
  assert.equal(f.calls.filter(call => call.method).length, 1);
});

test('failed provider operation retains its identity and exposes only read recovery', async t => {
  const f = fixture(t, { data: async (path, options) => { f.calls.push({ path, ...options }); throw Object.assign(new Error('upgrade reply lost'), { uncertain: true }); } });
  await providerOperation(f.owner(), entry, model.node, { label: 'Upgrade provider', suffix: '/upgrade', body: { version: 'v2' } }, () => {});
  assert.match(f.root.textContent, /Requested nodeworker one.*upgrade reply lost/s);
  await click(f.root, 'Inspect node jobs');
  assert.deepEqual(f.routes.at(-1), ['workloads', 'node/worker%20one/', 'jobs']);
  assert.equal(f.calls.length, 1);
});

test('service reads and mutation acceptance use the raw response contract', async t => {
  const f = fixture(t, { request: async (path, options = {}) => { f.calls.push({ path, ...options }); return options.method ? { job_id: 'service-job', node: model.node } : { managed: true, running: false, healthy: false, supervisor: 'zzRouter' }; }, data: async path => { f.calls.push({ path }); return { job_id: 'service-job', phase: 'done' }; } });
  await providerService(f.owner(), entry, model.node, () => {});
  await click(f.root, 'Start service');
  assert.equal(f.calls[0].path, '/zzrouter/v1/providers/engine/service/status?node=worker%20one');
  assert.equal(f.calls[1].path, '/zzrouter/v1/providers/engine/service/start?node=worker%20one');
  assert.equal(f.calls[2].path, '/jobs/service-job?node=worker%20one');
});

test('remote Ollama credentials clear on uncertain create, and inspect never replays the create', async t => {
  const f = fixture(t, { data: async (path, options) => { f.calls.push({ path, ...options }); throw Object.assign(new Error('connection reply lost'), { uncertain: true }); } });
  let inspected = 0; remoteOllama(f.owner(), () => { inspected++; });
  named(f.root, 'remote_name').value = 'remote'; named(f.root, 'remote_endpoint').value = 'http://example.test:11434';
  const token = named(f.root, 'remote_token'); token.value = 'synthetic';
  await submit(f.root);
  assert.equal(token.value, ''); assert.deepEqual(f.calls[0].body, { type: 'ollama-connect', name: 'remote', endpoint: 'http://example.test:11434', token: 'synthetic' });
  await click(f.root, 'Inspect providers'); assert.equal(inspected, 1); assert.equal(f.calls.length, 1);
});

test('version pin is coordinator-scoped while upgrade remains exact-node scoped', async t => {
  const f = fixture(t, { data: async (path, options = {}) => { f.calls.push({ path, ...options }); return { pinned: 'v1', nodes: [] }; } });
  await providerVersions(f.owner(), entry, model.node, () => {});
  named(f.root, 'provider_pin').value = 'v2'; await submit(f.root);
  const write = f.calls.find(call => call.method === 'PATCH');
  assert.equal(write.path, '/providers/engine'); assert.deepEqual(write.body, { pinned_version: 'v2' });
  named(f.root, 'upgrade_version').value = ''; await click(f.root, 'Review upgrade');
  const upgrade = f.calls.find(call => call.method === 'POST');
  assert.equal(upgrade.path, '/providers/engine/upgrade?node=worker%20one'); assert.deepEqual(upgrade.body, { version: '' });
});
