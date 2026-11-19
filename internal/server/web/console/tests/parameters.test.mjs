import test from 'node:test';
import assert from 'node:assert/strict';
import { parameterSettings, parameterQuery, parameterPatch, typedParameterValue, parameterConflicts } from '../parameters.mjs';
import { details } from '../details.mjs';
import { APIError } from '../api.mjs';
import { installDOM, named, find, button, drain, walk } from './dom-fixture.mjs';

const schema = { parameters: { ctx: { type: 'int', description: 'Context size' }, enabled: { type: 'bool' }, alias: { type: 'string' }, automatic: { type: 'int', auto: 'auto' } }, environment: {}, endpoints: { embeddings: { parameters: { ctx: { type: 'int' } } } } };
function fixture(t, handle) {
  const root = installDOM(t), controller = new AbortController(), calls = [];
  let exact = { provider: 'engine', scope: 'defaults', available_endpoints: ['chat', 'embeddings'], parameters: { ctx: '4096' }, environment: { TOKEN: 'test-private-value' } };
  const p = { root, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, go() {}, syncActivity() {}, api: {
    canLeave: () => true, canChange: () => true,
    request: async (path, options = {}) => {
      calls.push({ path, ...options });
      if (handle) { const result = await handle(path, options, calls); if (result !== undefined) return result; }
      if (options.method) return {};
      if (path.endsWith('/schema')) return structuredClone(schema);
      if (path.includes('/resolved')) return { parameters: { ctx: { value: 8192, tier: 'node', node: 'coordinator' } }, environment: { TOKEN: { value: 'test-effective-private-value', tier: 'node' } } };
      if (path.includes('/parameters?')) return structuredClone(exact);
      assert.fail(path);
    },
  } };
  const owner = details(p, 'Provider', () => {});
  t.after(() => controller.abort());
  return { root, controller, p: owner, calls, exact: value => { exact = value; } };
}
const form = root => find(root, item => item.tag === 'form' && item.className === 'entity-editor parameter-editor');
const submit = root => form(root).listeners.get('submit')({ preventDefault() {} });
const change = (root, name, value) => { const control = named(root, name); control.value = value; control.dispatchEvent(new Event('input')); return control; };

test('parameter patches address all four exact tiers without copying inherited values', () => {
  const changes = { parameters: { ctx: '12', alias: null }, environment: { TOKEN: '' } };
  const leaf = { parameters: { ctx: 12, alias: null }, environment: { TOKEN: '' } };
  const all = [
    ['defaults', { defaults: leaf }], ['model', { models: { 'model/a': leaf } }],
    ['node', { nodes: { worker: leaf } }], ['node-model', { models: { 'model/a': {} }, nodes: { worker: { models: { 'model/a': leaf } } } }],
  ];
  for (const [scope, expected] of all) assert.deepEqual(parameterPatch({ scope, node: 'worker', model: 'model/a' }, changes, schema), expected);
  assert.deepEqual(parameterPatch({ scope: 'node-model', node: 'worker', model: 'model/a', endpoint: 'embeddings' }, changes, schema), { models: { 'model/a': {} }, nodes: { worker: { models: { 'model/a': { endpoints: { embeddings: leaf } } } } } });
  assert.equal(parameterQuery({ scope: 'defaults', node: 'not-sent', model: 'not-sent' }), 'scope=defaults');
  assert.equal(parameterQuery({ scope: 'node-model', node: 'worker/a', model: 'model/a', endpoint: 'embeddings' }), 'scope=node-model&node=worker%2Fa&model=model%2Fa&endpoint=embeddings');
});

test('typed parameter serialization retains auto, empty strings and unsafe numbers', () => {
  assert.equal(typedParameterValue('0', { type: 'int' }), 0);
  assert.equal(typedParameterValue('false', { type: 'bool' }), false);
  assert.equal(typedParameterValue('0.25', { type: 'float' }), 0.25);
  assert.equal(typedParameterValue('', { type: 'int' }), '');
  assert.equal(typedParameterValue('9007199254740993', { type: 'int' }), '9007199254740993');
  assert.equal(typedParameterValue('auto', { type: 'int', auto: 'auto' }), 'auto');
  assert.equal(typedParameterValue(null, { type: 'int' }), null);
  assert.equal(typedParameterValue('NaN', { type: 'float' }), 'NaN');
  assert.equal(typedParameterValue('false', {}), 'false');
});

test('concurrent editing compares changed exact leaves, including absent versus empty', () => {
  const before = { parameters: { a: '', b: '1' }, environment: { TOKEN: 'old' } };
  assert.deepEqual(parameterConflicts(before, { parameters: { a: '', b: '2' } }, { parameters: { a: null } }), []);
  assert.deepEqual(parameterConflicts(before, { parameters: {} }, { parameters: { a: 'x' } }), ['parameters.a']);
  assert.deepEqual(parameterConflicts(before, { ...before, environment: { TOKEN: 'new' } }, { environment: { TOKEN: null } }), ['environment.TOKEN']);
});

test('editor displays configured defaults independently of effective node winners', async t => {
  const f = fixture(t); await parameterSettings(f.p, { provider: 'engine' });
  assert.equal(named(f.root, 'parameters_ctx').value, '4096');
  assert.match(named(f.root, 'parameter_endpoint').textContent, /chat/, 'valid endpoint need not have a narrowed schema');
  assert.match(f.root.textContent, /Effective: 8192 · node · coordinator/);
  change(f.root, 'parameters_ctx', '12288'); await submit(f.root);
  const write = f.calls.find(call => call.method === 'PATCH');
  assert.equal(write.contentType, 'application/merge-patch+json');
  assert.deepEqual(write.body, { defaults: { parameters: { ctx: 12288 } } });
  assert.equal(f.calls.filter(call => call.path.includes('/parameters?')).length, 3, 'initial, before write, after write');
});

test('environment values reveal deliberately, clear on hide and clear on panel exit', async t => {
  const f = fixture(t); await parameterSettings(f.p, { provider: 'engine' });
  const secret = named(f.root, 'environment_TOKEN');
  assert.equal(secret.value, ''); assert.ok(secret.attributes.has('data-sensitive'));
  assert.doesNotMatch(f.root.textContent, /test-private|test-effective-private/);
  button(f.root, 'Reveal').dispatchEvent(new Event('click'));
  assert.equal(secret.value, 'test-private-value');
  button(f.root, 'Hide').dispatchEvent(new Event('click')); assert.equal(secret.value, '');
  button(f.root, 'Reveal').dispatchEvent(new Event('click'));
  f.controller.abort(); assert.equal(secret.value, '');
});

test('unsetting an override sends null while preserving concealed unrelated environment', async t => {
  const f = fixture(t); await parameterSettings(f.p, { provider: 'engine', node: 'worker/a', model: 'model/a' });
  const toggle = named(f.root, 'parameters_ctx_override'); toggle.checked = false; toggle.dispatchEvent(new Event('change'));
  await submit(f.root);
  assert.deepEqual(f.calls.find(call => call.method === 'PATCH').body, { models: { 'model/a': {} }, nodes: { 'worker/a': { models: { 'model/a': { parameters: { ctx: null } } } } } });
  assert.ok(f.calls.some(call => call.path.includes('scope=node-model&node=worker%2Fa&model=model%2Fa')));
});

test('changed stored values block save and cannot be resubmitted as stale evidence', async t => {
  let exactReads = 0;
  const f = fixture(t, async path => {
    if (path.includes('/parameters?') && ++exactReads > 1) return { parameters: { ctx: '16384' }, environment: {} };
  });
  await parameterSettings(f.p, { provider: 'engine' }); change(f.root, 'parameters_ctx', '12288');
  await submit(f.root); await submit(f.root);
  assert.equal(f.calls.filter(call => call.method).length, 0);
  assert.match(f.root.textContent, /changed in another session/);
  assert.equal(button(f.root, 'Save settings').disabled, true);
  assert.ok(button(f.root, 'Inspect current settings'));
});

test('server-canonical evidence catches a concurrent edit through a stored flag alias', async t => {
  let exactReads = 0;
  const f = fixture(t, async path => {
    if (path.endsWith('/schema')) return { parameters: { 'ctx-size': { type: 'int' } }, environment: {} };
    // The coordinator projects the stored ctx_size alias to ctx-size on each
    // read. Its Go regression covers that projection across every scope.
    if (path.includes('/parameters?')) return { parameters: { 'ctx-size': ++exactReads === 1 ? '4096' : '16384' }, environment: {} };
    if (path.includes('/resolved')) return { parameters: { 'ctx-size': { value: '4096', tier: 'defaults' } }, environment: {} };
  });
  await parameterSettings(f.p, { provider: 'engine' });
  assert.equal(named(f.root, 'parameters_ctx-size').value, '4096');
  assert.equal(walk(f.root).some(item => item.getAttribute('name') === 'parameters_ctx_size'), false, 'one editor for one server-owned flag identity');
  change(f.root, 'parameters_ctx-size', '8192'); await submit(f.root);
  assert.equal(f.calls.filter(call => call.method).length, 0);
  assert.match(f.root.textContent, /parameters.ctx-size/);
  assert.equal(button(f.root, 'Save settings').disabled, true);
});

test('ambiguous stored flag aliases fail closed before editing or saving', async t => {
  const initial = fixture(t, async path => {
    if (path.includes('/parameters?')) throw new APIError('Conflicting parameter spellings at this scope', { status: 409, errors: [{ key: 'ctx_size', code: 'duplicate_flag' }] });
  });
  await parameterSettings(initial.p, { provider: 'engine' });
  assert.equal(walk(initial.root).some(item => item.tag === 'form'), false);
  let exactReads = 0;
  const concurrent = fixture(t, async path => {
    if (path.includes('/parameters?') && ++exactReads > 1) throw new APIError('Conflicting parameter spellings at this scope', { status: 409, errors: [{ key: 'ctx_size', code: 'duplicate_flag' }] });
  });
  await parameterSettings(concurrent.p, { provider: 'engine' });
  change(concurrent.root, 'parameters_ctx', '8192'); await submit(concurrent.root);
  assert.equal(concurrent.calls.filter(call => call.method).length, 0);
});

test('uncertain parameter writes remain locked for inspect-only recovery', async t => {
  const f = fixture(t, async (_path, options) => { if (options.method) throw new APIError('Response lost', { uncertain: true }); });
  await parameterSettings(f.p, { provider: 'engine' }); change(f.root, 'parameters_ctx', '12288');
  await submit(f.root); await submit(f.root);
  assert.equal(f.calls.filter(call => call.method).length, 1);
  assert.equal(button(f.root, 'Save settings').disabled, true);
});

test('failed exact read after a saved write never restores the old editor', async t => {
  let written = false;
  const f = fixture(t, async (path, options) => {
    if (options.method) { written = true; return {}; }
    if (written && path.includes('/parameters?')) throw new Error('Read unavailable');
  });
  await parameterSettings(f.p, { provider: 'engine' }); change(f.root, 'parameters_ctx', '12288'); await submit(f.root);
  assert.ok(button(f.root, 'Reload settings'));
  assert.equal(walk(f.root).some(item => item.tag === 'button' && item.textContent === 'Save settings'), false);
});

test('schema failure disables editing while optional provenance failure does not', async t => {
  const failed = fixture(t, async path => { if (path.endsWith('/schema')) throw new Error('Schema unavailable'); });
  await parameterSettings(failed.p, { provider: 'engine' });
  assert.equal(walk(failed.root).some(item => item.tag === 'form'), false);
  const provenance = fixture(t, async path => { if (path.includes('/resolved')) throw new Error('Node offline'); });
  await parameterSettings(provenance.p, { provider: 'engine' });
  assert.equal(named(provenance.root, 'parameters_ctx').value, '4096');
  assert.match(provenance.root.textContent, /Effective values could not be read/);
});

test('endpoint scope transitions use explicit scope reads and preserve no stale controls', async t => {
  const f = fixture(t); await parameterSettings(f.p, { provider: 'engine', node: 'worker' });
  named(f.root, 'parameter_scope').value = 'model'; named(f.root, 'parameter_scope').dispatchEvent(new Event('change'));
  named(f.root, 'parameter_model').value = 'model/a'; named(f.root, 'parameter_endpoint').value = 'embeddings';
  const context = find(f.root, item => item.tag === 'form' && item.className === 'parameter-context');
  await context.listeners.get('submit')({ preventDefault() {} }); await drain();
  assert.ok(f.calls.some(call => call.path.endsWith('/parameters?scope=model&model=model%2Fa&endpoint=embeddings')));
  assert.equal(named(f.root, 'parameter_node').value, '');
});
