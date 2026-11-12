import test from 'node:test';
import assert from 'node:assert/strict';
import { API, Lifetime, Obsolete, changedFields, applySummary } from '../api.mjs';
import { editableValues } from '../access.mjs';

const origin = 'https://router.example';
const bootstrap = (csrf, authenticated = true) => ({ data: { authenticated, csrf, issuer: origin, node_name: 'Fixture' } });
const identity = id => ({ data: { id, name: id, role: 'admin' } });
const response = (body, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => body });
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; };
function queued(...bodies) {
  const calls = [];
  return { calls, fetcher: async (path, options) => { calls.push({ path, options }); assert.ok(bodies.length, 'unexpected request'); return response(bodies.shift()); } };
}

test('default fetch keeps its native Window receiver', async () => {
  const previous = globalThis.fetch;
  globalThis.fetch = function () {
    assert.equal(this, globalThis, 'the browser throws Illegal invocation for an API receiver');
    return Promise.resolve(response(bootstrap('anonymous', false)));
  };
  try { assert.equal((await new API({ origin }).refresh()).csrf, 'anonymous'); }
  finally { globalThis.fetch = previous; }
});

test('refresh clears authority before reading and atomically installs a coherent identity', async () => {
  const queue = queued(bootstrap('A'), identity('alice'), bootstrap('A'));
  let clears = 0;
  const api = new API({ ...queue, origin, cleared: () => { clears++; assert.equal(api.state, null); assert.equal(queue.calls.length, 0); } });
  const pending = api.refresh();
  assert.equal(api.state, null);
  const state = await pending;
  assert.equal(clears, 1);
  assert.equal(state.identity.id, 'alice');
  assert.equal(state.csrf, 'A');
  assert.deepEqual(queue.calls.map(call => call.path), ['/zzrouter/v1/session', '/zzrouter/v1/me', '/zzrouter/v1/session']);
  assert.equal(queue.calls[0].options.credentials, 'same-origin');
  assert.equal(queue.calls[0].options.redirect, 'error');
});

test('bootstrap A, identity B, bootstrap B never installs mixed authority', async () => {
  const queue = queued(bootstrap('A'), identity('bob'), bootstrap('B'));
  const api = new API({ ...queue, origin });
  await assert.rejects(api.refresh(), error => error.code === 'session_changed');
  assert.equal(api.state, null);
  await assert.rejects(api.data('/keys', { method: 'POST', body: {} }), error => error.code === 'session_refresh_required');
  assert.equal(queue.calls.length, 3);
});

test('a late obsolete refresh cannot replace the current identity', async () => {
  const slow = deferred();
  const queue = queued(bootstrap('B'), identity('bob'), bootstrap('B'));
  let first = true;
  const api = new API({ origin, fetcher: (...args) => { if (first) { first = false; return slow.promise; } return queue.fetcher(...args); } });
  const old = api.refresh();
  const fresh = await api.refresh();
  slow.resolve(response(bootstrap('old-anonymous', false)));
  await assert.rejects(old, Obsolete);
  assert.equal(api.state, fresh);
  assert.equal(api.state.identity.id, 'bob');
});

test('a late 401 from identity A cannot clear newly installed identity B', async () => {
  const slow = deferred();
  const queue = queued(bootstrap('A'), identity('alice'), bootstrap('A'), bootstrap('B'), identity('bob'), bootstrap('B'));
  let rejected = 0;
  const api = new API({ origin, fetcher: (path, opts) => path.endsWith('/keys') ? slow.promise : queue.fetcher(path, opts), rejected: () => { rejected++; api.clear(); } });
  await api.refresh();
  const old = api.data('/keys');
  await api.refresh();
  slow.resolve(response({ detail: 'Old identity expired' }, 401));
  await assert.rejects(old, error => error.status === 401);
  assert.equal(rejected, 0);
  assert.equal(api.state.identity.id, 'bob');
});

test('a late forbidden response from an abandoned page has no global side effect', async () => {
  const slow = deferred();
  const lifetime = new Lifetime();
  const oldPage = lifetime.next();
  let rejected = false;
  const api = new API({ origin, fetcher: () => slow.promise, rejected: () => { rejected = true; } });
  const old = api.data('/keys', { signal: oldPage.signal });
  const currentPage = lifetime.next();
  slow.resolve(response({ detail: 'Forbidden' }, 403));
  await assert.rejects(old);
  assert.equal(rejected, false);
  assert.equal(oldPage.current(), false);
  assert.equal(currentPage.current(), true);
  lifetime.end();
  assert.equal(currentPage.current(), false);
});

test('successful late data is ignored by its invalidated page owner', async () => {
  const lifetime = new Lifetime();
  const oldPage = lifetime.next();
  const slow = deferred();
  let rendered;
  const api = new API({ origin, fetcher: () => slow.promise });
  const read = api.data('/keys', { signal: oldPage.signal }).then(value => { if (oldPage.current()) rendered = value; });
  lifetime.next();
  slow.resolve(response({ data: ['obsolete'] }));
  await read;
  assert.equal(rendered, undefined);
});

test('wrong issuer never enables a form or sends a credential', async () => {
  const queue = queued(bootstrap('A'));
  const api = new API({ ...queue, origin: 'https://alias.example' });
  const state = await api.refresh();
  assert.equal(state.mismatch, true);
  assert.equal(state.identity, null);
  assert.equal(state.csrf, null);
  await assert.rejects(api.data('/session', { method: 'POST', body: { key: 'synthetic' } }));
  assert.equal(queue.calls.length, 1);
});

test('unknown mutation completion is never retried and retains no request body', async () => {
  let count = 0;
  const api = new API({ origin, fetcher: async () => { count++; throw new TypeError('network interrupted'); } });
  api.state = { csrf: 'proof' };
  await assert.rejects(api.data('/keys', { method: 'POST', body: { id: 'fixture', key: 'synthetic' } }), error => error.uncertain);
  assert.equal(count, 1);
  assert.equal(JSON.stringify(api).includes('synthetic'), false);
});

test('known refused persistence is distinguishable from an uncertain access write', async () => {
  for (const [code, uncertain] of [['provider_connection_persistence_failed', false], ['internal_error', true]]) {
    const api = new API({ origin, fetcher: async () => response({ code, detail: 'Failed' }, 503) });
    api.state = { csrf: 'proof' };
    await assert.rejects(api.data('/providers/openai/connection', { method: 'PUT', body: {} }), error => error.uncertain === uncertain);
  }
});

test('explicit authorization refusal is settled before the auth-reset callback', async () => {
  const api = new API({ origin, fetcher: async () => response({ detail: 'No access' }, 403), rejected() {
    assert.equal(api.pending.size, 0, 'the known refusal is no longer an in-flight change');
    api.clear();
  } });
  api.state = { csrf: 'proof' };
  await assert.rejects(api.data('/keys', { method: 'POST', body: {} }), error => error.status === 403 && !error.uncertain);
  assert.equal(api.unknownChange, false);
});

test('diagnostic POST needs CSRF but cancellation never creates durable-write uncertainty', async () => {
  const slow = deferred();
  const page = new Lifetime();
  const scope = page.next();
  const api = new API({ origin, fetcher: () => slow.promise });
  api.state = { csrf: 'proof' };
  const pending = api.data('/providers/openai/connection/test', { method: 'POST', effectful: false, signal: scope.signal });
  assert.equal(api.canLeave(), true);
  api.clear();
  page.end();
  slow.resolve({ ok: true, status: 200, json: async () => { throw new SyntaxError('interrupted body'); } });
  await assert.rejects(pending, error => !error.uncertain);
  assert.equal(api.unknownChange, false);
  await assert.rejects(api.data('/providers/openai/connection/test', { method: 'POST', effectful: false }), error => error.code === 'session_refresh_required');
});

test('a probe failure blocks new mutation transport without asserting an unknown write outcome', async () => {
  let sends = 0;
  const api = new API({ origin, fetcher: async () => { sends++; return response({ data: {} }); } });
  api.state = { csrf: 'proof' };
  api.unverified = true;
  await assert.rejects(api.data('/keys', { method: 'POST', body: {} }), /Verify your session/);
  assert.equal(sends, 0);
  assert.equal(api.pending.size, 0);
  assert.equal(api.unknownChange, false);
});

test('invalid JSON envelope after a durable write retains a generic unknown-outcome notice', async () => {
  const api = new API({ origin, fetcher: async () => response(null) });
  api.state = { csrf: 'proof' };
  await assert.rejects(api.data('/keys', { method: 'POST', body: {} }), error => error.uncertain);
  assert.equal(api.canLeave(), true);
  api.clear();
  assert.equal(api.unknownChange, true);
});

test('list adapter accepts data envelopes without success and follows pagination', async () => {
  const queue = queued({ data: ['one'], has_more: true }, { data: ['two'], has_more: false });
  const api = new API({ ...queue, origin });
  assert.deepEqual(await api.list('/providers/catalog?kind=all'), ['one', 'two']);
  assert.equal(queue.calls[1].path, '/zzrouter/v1/providers/catalog?kind=all&offset=1');
  const broken = new API({ ...queued({ data: [], has_more: true }), origin });
  await assert.rejects(broken.list('/keys'), /fully loaded/);
});

test('partial save presentation reports every boolean independently', () => {
  const summary = applySummary({ saved: true, durable: false, active: false, published: true, outcome: 'saved_durability_uncertain' });
  assert.match(summary, /Saved/);
  assert.match(summary, /durability not confirmed/);
  assert.match(summary, /connection not active/);
  assert.match(summary, /catalog published/);
});

test('access patches include only edited fields and never re-send membership or suspension', () => {
  const record = { id: 'fixture', name: 'Fixture', role: 'user', team_id: 'readers', suspended: true, metadata: { other: 'keep' }, rpm_limit: 10 };
  const before = editableValues(record, 'keys');
  assert.deepEqual(changedFields(before, { ...before, rpm_limit: 20 }), { rpm_limit: 20 });
  assert.equal('team_id' in before, false);
  assert.equal('suspended' in before, false);
  assert.equal('metadata' in before, false);
});
