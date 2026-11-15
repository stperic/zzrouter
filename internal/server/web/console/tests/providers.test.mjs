import test from 'node:test';
import assert from 'node:assert/strict';
import { providers } from '../providers.mjs';
import { find, named, button, walk, drain, installDOM } from './dom-fixture.mjs';

const schema = { managed_available: true, revision: 7, scope: 'coordinator', mode: 'managed', configured: false, enabled: false, fields: [{ id: 'token', label: 'Token', secret: true, required: true }] };
const catalog = [
  { key: 'cloud-a', name: 'Cloud A', kind: 'cloud', is_cloud: true, available: false, connection_supported: true },
  { key: 'engine-a', name: 'Engine A', kind: 'external', connection_supported: true },
  { key: 'huggingface', name: 'Hugging Face', kind: 'registry', connection_supported: true },
];
function fixture(t, overrides = {}) {
  const root = installDOM(t), controller = new AbortController(), calls = [], routes = [];
  let select;
  t.after(() => controller.abort());
  const p = { root, current: () => !controller.signal.aborted, scope: { signal: controller.signal }, state: { node_name: 'atlas' }, syncActivity() {},
    handleSelection: fn => { select = fn; }, go: async(...route) => { routes.push(route); if (p.api.canLeave()) await select(route[1]); },
    api: { canLeave: () => true, canChange: () => true,
      list: async path => path.startsWith('/providers/catalog') ? catalog : path === '/nodes' ? [
        { name: 'atlas', health_status: 'healthy', providers: [] }, { name: 'spruce', health_status: 'down' },
      ] : path === '/providers' ? [{ name: 'engine-a', node: 'atlas', kind: 'external', enabled: false, installed: true }] : [],
      data: async (path, options = {}) => { calls.push({ path, ...options }); return schema; }, ...overrides,
    } };
  return { root, p, controller, calls, routes };
}
async function choose(f, key, edit = false) {
  find(f.root, node => node.dataset.provider === key).dispatchEvent(new Event('click')); await drain();
  const action = walk(f.root).find(node => node.tag === 'button' && ['Edit', 'Connect'].includes(node.textContent));
  if (edit && action) { action.dispatchEvent(new Event('click')); await drain(); }
}
async function submit(root) { find(root, node => node.tag === 'form').dispatchEvent(new Event('submit', { cancelable: true })); await drain(); }

test('provider lists do not automatically select or expose credentials; categories remain separate', async t => {
  const f = fixture(t); await providers(f.p);
  assert.deepEqual(walk(f.root).filter(node => node.dataset.provider).map(node => node.dataset.provider), ['engine-a']);
  assert.equal(walk(f.root).some(node => node.tag === 'dialog'), false);
  f.root.replaceChildren(); await providers(f.p, 'registries');
  assert.deepEqual(walk(f.root).filter(node => node.dataset.provider).map(node => node.dataset.provider), ['huggingface']);
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
});

test('selection opens node and connection details; editing is explicit and Cancel restores details', async t => {
  const f = fixture(t); await providers(f.p, 'local'); await choose(f, 'engine-a');
  assert.match(f.root.textContent, /atlas.*Healthy.*DisabledInstalled.*spruce.*Down/s);
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
  button(f.root, 'Connect').dispatchEvent(new Event('click'));
  const token = named(f.root, 'token'); token.value = 'synthetic-candidate';
  button(f.root, 'Cancel').dispatchEvent(new Event('click'));
  assert.equal(token.value, ''); assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
  assert.equal(find(f.root, node => node.dataset.provider === 'engine-a').getAttribute('aria-current'), 'true');
});

test('connection read failure retains independent local node observations', async t => {
  const f = fixture(t, { data: async () => { throw new Error('connection unavailable'); } });
  await providers(f.p, 'local', 'engine-a');
  assert.match(f.root.textContent, /connection unavailable.*atlas.*Healthy.*spruce.*Down/s);
});

for (const category of ['local', 'cloud', 'registries']) {
  test(`unsupported ${category} connections explain their real configuration owner`, async t => {
    const f = fixture(t); const list = f.p.api.list;
    f.p.api.list = path => path.startsWith('/providers/catalog') ? catalog.map(item => ({ ...item, connection_supported: false })) : list(path);
    const key = { local: 'engine-a', cloud: 'cloud-a', registries: 'huggingface' }[category];
    await providers(f.p, category, key);
    assert.match(f.root.textContent, category === 'local' ? /Runtime management.*Manage runtime/s : /Configure this connection on the coordinator/);
    assert.equal(f.calls.length, 0); assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
  });
}

for (const [key, category, activate] of [['cloud-a', 'cloud', false], ['cloud-a', 'cloud', true], ['huggingface', 'registries', false]]) {
  test(`${key} saves one conditional replacement, activation=${activate}, then returns to refreshed details`, async t => {
    let revision = 7, configured = false; const writes = [];
    const f = fixture(t, { data: async (path, options = {}) => {
      if (!options.method) return { ...schema, revision, configured };
      writes.push({ path, ...options }); revision++; configured = true;
      return { saved: true, durable: true, active: activate, published: true, outcome: 'saved' };
    } });
    await providers(f.p, category); await choose(f, key, true);
    const token = named(f.root, 'token'); token.value = 'synthetic-candidate';
    if (category === 'cloud' && !activate) { button(f.root, 'Save without enabling').dispatchEvent(new Event('click')); await drain(); }
    else await submit(f.root);
    assert.equal(writes.length, 1); assert.equal(writes[0].method, 'PUT');
    assert.deepEqual(writes[0].body, { expected_revision: 7, values: { token: 'synthetic-candidate' }, activate });
    assert.equal(token.value, ''); assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
    assert.ok(button(f.root, 'Edit')); assert.match(f.root.textContent, /Configured/);
  });
}

test('removing credentials reads current status and preserves selected provider', async t => {
  let configured = true; const writes = [];
  const f = fixture(t, { data: async (path, options = {}) => {
    if (!options.method) return { ...schema, configured };
    writes.push(options); configured = false; return { saved: true, durable: true, published: true, outcome: 'saved' };
  } });
  await providers(f.p, 'cloud'); await choose(f, 'cloud-a', true);
  button(f.root, 'Remove credentials').dispatchEvent(new Event('click')); await drain();
  assert.equal(writes[0].method, 'DELETE'); assert.equal(writes[0].body.expected_revision, 7);
  assert.ok(button(f.root, 'Connect')); assert.match(find(f.root, node => node.dataset.provider === 'cloud-a').textContent, /Set up/);
});

test('a saved connection with failed readback offers only a read retry and keeps candidates scrubbed', async t => {
  let saved = false, fail = true, writes = 0;
  const f = fixture(t, { data: async (_path, options = {}) => {
    if (options.method) { saved = true; writes++; return { saved: true, durable: true, published: true, outcome: 'saved' }; }
    if (saved && fail) throw new Error('read unavailable');
    return { ...schema, configured: saved };
  } });
  await providers(f.p, 'cloud'); await choose(f, 'cloud-a', true);
  const token = named(f.root, 'token'); token.value = 'synthetic'; await submit(f.root);
  assert.equal(token.value, ''); assert.match(f.root.textContent, /Saved.*read unavailable/s);
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
  fail = false; button(f.root, 'Reload details').dispatchEvent(new Event('click')); await drain();
  assert.equal(writes, 1); assert.ok(button(f.root, 'Edit'));
});

for (const outcome of ['saved', 'saved_activation_failed']) {
  test(`provider readback retains the ${outcome} result independently of current activation`, async t => {
    const f = fixture(t, { data: async (_path, options = {}) => options.method ? { saved: true, durable: true, active: true, published: true, outcome } : { ...schema, configured: true } });
    await providers(f.p, 'cloud'); await choose(f, 'cloud-a', true);
    named(f.root, 'token').value = 'synthetic'; await submit(f.root);
    const notice = find(f.root, node => node.className?.includes('notice') && node.textContent.includes('catalog published'));
    assert.match(notice.textContent, /connection active/);
    if (outcome === 'saved_activation_failed') { assert.match(notice.textContent, /activation reported an error/); assert.match(notice.className, /warning/); }
    else assert.doesNotMatch(notice.className, /warning/);
  });
}

test('obsolete connection details cannot reopen after selection changes', async t => {
  let resolve; const held = new Promise(done => { resolve = done; });
  const f = fixture(t, { data: () => held }); await providers(f.p, 'cloud');
  find(f.root, node => node.dataset.provider === 'cloud-a').dispatchEvent(new Event('click')); await drain();
  const panel = find(f.root, node => node.tag === 'dialog');
  button(f.root, '×').dispatchEvent(new Event('click')); await drain();
  resolve(schema); await drain(); assert.equal(f.root.contains(panel), false);
});

test('worker-only provider never falls back to an unrelated coordinator editor', async t => {
  const f = fixture(t, { data: () => assert.fail('unrelated credentials must not load') });
  await providers(f.p, 'cloud', 'worker-only');
  assert.match(f.root.textContent, /not defined on the coordinator/);
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
});

test('overlapping connection read retries cannot replace a newer credential draft', async t => {
  let failed = true; const waiting = [];
  const f = fixture(t, { data: async () => {
    if (failed) throw new Error('Unavailable');
    return new Promise(resolve => waiting.push(resolve));
  } });
  await providers(f.p, 'cloud', 'cloud-a'); failed = false;
  const retry = button(f.root, 'Reload details');
  retry.dispatchEvent(new Event('click')); retry.dispatchEvent(new Event('click')); await drain();
  assert.equal(waiting.length, 2);
  waiting[1](schema); await drain();
  const token = named(f.root, 'token'); token.value = 'Keep this candidate';
  find(f.root, node => node.tag === 'form').dispatchEvent(new Event('input'));
  waiting[0](schema); await drain();
  assert.equal(named(f.root, 'token'), token); assert.equal(token.value, 'Keep this candidate');
});

test('first-time cloud setup opens directly and makes activation explicit without a checkbox', async t => {
  const f = fixture(t); await providers(f.p, 'cloud', 'cloud-a');
  assert.equal(named(f.root, 'token').type, 'password');
  assert.ok(button(f.root, 'Save and enable'));
  assert.ok(button(f.root, 'Test connection'));
  assert.equal(walk(f.root).some(node => node.type === 'checkbox'), false);
  const advanced = find(f.root, node => node.tag === 'details' && node.textContent.includes('Save without enabling'));
  assert.equal(advanced.getAttribute('open'), undefined);
  assert.doesNotMatch(f.root.textContent, /Connect after saving|Test candidate/);
});

test('updating an enabled provider preserves enablement without another activation', async t => {
  const writes = [];
  const f = fixture(t, { data: async (_path, options = {}) => {
    if (!options.method) return { ...schema, configured: true, enabled: true, present: ['token'] };
    writes.push(options); return { saved: true, durable: true, active: true, published: true, outcome: 'saved' };
  } });
  await providers(f.p, 'cloud', 'cloud-a');
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
  button(f.root, 'Edit').dispatchEvent(new Event('click'));
  assert.equal(named(f.root, 'token').value, '');
  assert.ok(button(f.root, 'Save changes'));
  named(f.root, 'token').value = 'replacement'; await submit(f.root);
  assert.equal(writes.length, 1); assert.equal(writes[0].body.activate, false);
  assert.match(f.root.textContent, /Credentials saved\./);
  const receipt = find(f.root, node => node.tag === 'details' && node.textContent.includes('credential durability confirmed'));
  assert.equal(receipt.getAttribute('open'), undefined);
});

test('saved credentials can be enabled without re-entering or resending their values', async t => {
  let enabled = false; const writes = [];
  const f = fixture(t, { data: async (path, options = {}) => {
    if (!options.method) return { ...schema, configured: true, enabled, present: ['token'] };
    writes.push({ path, ...options }); enabled = true; return { enabled: true };
  } });
  await providers(f.p, 'cloud', 'cloud-a');
  assert.equal(walk(f.root).some(node => node.tag === 'input'), false);
  button(f.root, 'Enable provider').dispatchEvent(new Event('click')); await drain();
  assert.equal(writes.length, 1); assert.equal(writes[0].path, '/providers/cloud-a');
  assert.equal(writes[0].method, 'PATCH'); assert.deepEqual(writes[0].body, { enabled: true });
  assert.match(f.root.textContent, /Provider enabled/);
  assert.equal(walk(f.root).some(node => node.tag === 'button' && node.textContent === 'Enable provider'), false);
});

test('an uncertain enable response reads current state without retrying the mutation', async t => {
  let enabled = false, writes = 0;
  const f = fixture(t, { data: async (_path, options = {}) => {
    if (!options.method) return { ...schema, configured: true, enabled };
    writes++; enabled = true; throw Object.assign(new Error('Enable outcome unknown'), { uncertain: true });
  } });
  const list = f.p.api.list;
  f.p.api.list = (path, options) => { assert.equal(options.signal.aborted, false, 'readback must use the active panel view'); return list(path, options); };
  await providers(f.p, 'cloud', 'cloud-a');
  button(f.root, 'Enable provider').dispatchEvent(new Event('click')); await drain();
  assert.equal(writes, 1); assert.match(f.root.textContent, /Enable outcome unknown.*Enabled/s);
  assert.equal(walk(f.root).some(node => node.tag === 'button' && node.textContent === 'Enable provider'), false);
});

test('cloud connection read failures show recovery without unrelated local-node inventory', async t => {
  const f = fixture(t, { data: async () => { throw new Error('Connection unavailable'); } });
  await providers(f.p, 'cloud', 'cloud-a');
  assert.match(f.root.textContent, /Connection unavailable/);
  assert.ok(button(f.root, 'Reload details'));
  assert.doesNotMatch(f.root.textContent, /spruce|Compute nodes/);
});

for (const [result, expected] of [
  [{ status: 'ok', code: 'accepted' }, /Connection verified\. Save to apply/],
  [{ status: 'failed', code: 'authentication_failed', http_status: 401 }, /key is complete, active.*HTTP 401.*Nothing was saved/s],
  [{ status: 'failed', code: 'authentication_failed', http_status: 403 }, /key’s permissions.*HTTP 403.*Nothing was saved/s],
  [{ status: 'failed', code: 'unreachable' }, /coordinator’s network connection.*Nothing was saved/s],
  [{ status: 'failed', code: 'rate_limited', http_status: 429 }, /Wait a moment.*HTTP 429.*Nothing was saved/s],
]) {
  test(`connection test ${result.http_status || result.code} preserves the candidate without saving`, async t => {
    const requests = [];
    const f = fixture(t, { data: async (path, options = {}) => {
      if (!options.method) return schema;
      requests.push({ path, ...options }); return result;
    } });
    await providers(f.p, 'cloud', 'cloud-a');
    const token = named(f.root, 'token'); token.value = 'synthetic-candidate';
    button(f.root, 'Test connection').dispatchEvent(new Event('click')); await drain();
    assert.match(f.root.textContent, expected);
    assert.equal(token.value, 'synthetic-candidate');
    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, '/providers/cloud-a/connection/test');
    assert.equal(requests[0].effectful, false);
    assert.deepEqual(requests[0].body, { values: { token: 'synthetic-candidate' } });
    assert.equal(button(f.root, 'Save and enable').disabled, false, 'a diagnostic must not become a prerequisite for saving');
    token.value = 'changed-candidate'; token.dispatchEvent(new Event('input'));
    assert.match(f.root.textContent, /Credentials changed\. Test again/);
    assert.doesNotMatch(f.root.textContent, /Connection verified|HTTP 401|HTTP 403|HTTP 429/);
  });
}

test('storage unavailable disables every save action while still allowing a diagnostic', async t => {
  const requests = [];
  const f = fixture(t, { data: async (_path, options = {}) => {
    if (!options.method) return { ...schema, managed_available: false };
    requests.push(options); return { status: 'ok', code: 'accepted' };
  } });
  await providers(f.p, 'cloud', 'cloud-a'); named(f.root, 'token').value = 'synthetic';
  assert.equal(button(f.root, 'Save and enable').disabled, true);
  assert.equal(button(f.root, 'Save without enabling').disabled, true);
  await submit(f.root);
  assert.equal(requests.length, 0);
  button(f.root, 'Test connection').dispatchEvent(new Event('click')); await drain();
  assert.equal(requests.length, 1); assert.equal(requests[0].method, 'POST');
  assert.equal(button(f.root, 'Save and enable').disabled, true);
});

test('revealed input remains sensitive and is scrubbed when the editor is dismissed', async t => {
  const f = fixture(t); await providers(f.p, 'cloud', 'cloud-a');
  const token = named(f.root, 'token'); token.value = 'synthetic-revealed-key';
  button(f.root, 'Show').dispatchEvent(new Event('click'));
  assert.equal(token.type, 'text');
  assert.equal(button(f.root, 'Hide').getAttribute('aria-pressed'), 'true');
  button(f.root, 'Hide').dispatchEvent(new Event('click'));
  assert.equal(token.type, 'password');
  button(f.root, 'Show').dispatchEvent(new Event('click'));
  button(f.root, 'Cancel').dispatchEvent(new Event('click'));
  assert.equal(token.value, '');
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
});

test('saving after revealing a key clears it and warns if requested activation is not confirmed', async t => {
  let configured = false;
  const f = fixture(t, { data: async (_path, options = {}) => {
    if (!options.method) return { ...schema, configured };
    configured = true; return { saved: true, durable: true, active: false, published: true, outcome: 'saved' };
  } });
  await providers(f.p, 'cloud', 'cloud-a');
  const token = named(f.root, 'token'); token.value = 'synthetic';
  button(f.root, 'Show').dispatchEvent(new Event('click')); await submit(f.root);
  assert.equal(token.value, '');
  assert.match(f.root.textContent, /saved, but needs attention.*connection not active/s);
  assert.doesNotMatch(f.root.textContent, /Provider enabled/);
});
