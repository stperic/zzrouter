import test from 'node:test';
import assert from 'node:assert/strict';
import { button, find, named, walk, drain, installDOM } from './dom-fixture.mjs';

const origin = 'https://router.example';
const response = (data, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => data });
const reply = data => response({ data });
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
const click = async (root, label) => { button(root, label).dispatchEvent(new Event('click')); await drain(); };
const submit = async root => { find(root, node => node.tag === 'form').dispatchEvent(new Event('submit', { cancelable: true })); await drain(); };
const focus = async () => { window.dispatchEvent(new Event('focus')); await drain(); };
let imports = 0;

async function consoleFixture(t, { own = false, managed = false, expiresAt, identityExpiry } = {}) {
  let channel;
  const clipboard = [];
  const app = installDOM(t, {
    BroadcastChannel: class extends EventTarget { constructor() { super(); channel = this; } postMessage() {} },
    navigator: { clipboard: { writeText: async value => { clipboard.push(value); } } },
  });
  const state = { authenticated: true, csrf: 'proof-A', issuer: origin, node_name: 'Fixture', expires_at: expiresAt };
  let identity = { id: own ? 'mine' : 'admin', name: 'Operator', role: 'admin', is_virtual: own, expires_at: identityExpiry, limits: {}, team: null, usage: null };
  const record = { id: 'mine', name: 'My administrator', role: 'admin' };
  const catalog = ['openai', 'groq'].map(key => ({ key, name: key, is_cloud: true, connection_supported: true }));
  const calls = [];
  const held = new Map();
  let failProbe = false;
  t.mock.method(globalThis, 'fetch', async (path, options) => {
    calls.push({ path, options });
    const method = options.method;
    const pending = held.get(`${method} ${path}`);
    if (pending) {
      pending.sent = true;
      pending.signal = options.signal;
      pending.commit?.();
      options.signal.addEventListener('abort', () => pending.reject(new Error('request canceled')), { once: true });
      return pending.promise;
    }
    if (path.endsWith('/session')) {
      if (method === 'GET' && failProbe) throw new Error('probe offline');
      if (method === 'POST') { state.authenticated = true; state.csrf = 'proof-new-login'; }
      if (method === 'DELETE') { state.authenticated = false; state.csrf = 'proof-logged-out'; }
      return reply({ ...state });
    }
    if (path.endsWith('/me')) return state.authenticated ? reply(identity) : response({ detail: 'No browser identity' }, 401);
    if (path === '/health') return response({ version: 'fixture' });
    if (path.includes('/providers/catalog')) return reply(catalog);
    if (path.endsWith('/me/connections') || path.endsWith('/teams')) return reply([]);
    if (path.endsWith('/keys') && method === 'GET') return reply(own || managed ? [record] : []);
    if (path.endsWith('/keys/mine') && method === 'GET') return reply(record);
    if (path.endsWith('/keys') && method === 'POST') return reply({ id: 'created', key: 'synthetic-created-key', role: 'user' });
    if (path.includes('/providers/') && path.endsWith('/connection')) {
      if (method !== 'GET') return reply({ saved: true, durable: true, active: true, published: true });
      return reply({ revision: 'revision-A', managed_available: true, mode: 'external', scope: 'coordinator', fields: [{ id: 'token', label: 'API key', secret: true, required: true }], values: {}, present: [] });
    }
    assert.fail(`unexpected fixture request: ${method} ${path}`);
  });
  await import(`../console.mjs?focus-fixture=${++imports}`);
  await drain();
  return {
    app, state, clipboard, calls,
    hold(method, path, commit) { const operation = { ...deferred(), commit }; held.set(`${method} /zzrouter/v1${path}`, operation); return operation; },
    unhold(method, path) { held.delete(`${method} /zzrouter/v1${path}`); },
    failProbe(value) { failProbe = value; },
    identity(value) { identity = value; },
    authChanged() { const event = new Event('message'); event.data = 'auth-state-changed'; channel.dispatchEvent(event); },
  };
}

async function createForm(fixture) {
  await click(fixture.app, 'Keys & teams');
  await click(fixture.app, 'Create key');
  named(fixture.app, 'id').value = 'created';
  named(fixture.app, 'name').value = 'Created key';
}
async function ownForm(fixture) { await click(fixture.app, 'Keys & teams'); await click(fixture.app, 'Manage'); }
async function provider(fixture, key) {
  find(fixture.app, node => node.dataset.provider === key).dispatchEvent(new Event('click'));
  await drain();
}

test('focus preserves the selected provider and exact masked candidate without installing another identity', async t => {
  const f = await consoleFixture(t);
  await click(f.app, 'Providers');
  await provider(f, 'groq');
  const candidate = named(f.app, 'token');
  candidate.value = 'synthetic-groq-candidate';
  const before = f.calls.length;
  await focus();
  assert.equal(named(f.app, 'token'), candidate);
  assert.equal(candidate.value, 'synthetic-groq-candidate');
  assert.equal(find(f.app, node => node.dataset.provider === 'groq').getAttribute('aria-current'), 'true');
  assert.deepEqual(f.calls.slice(before).map(call => call.path), ['/zzrouter/v1/session']);
});

test('created key survives clipboard and password-manager focus; pagehide still scrubs it', async t => {
  const f = await consoleFixture(t);
  await createForm(f);
  await submit(f.app);
  const secret = named(f.app, 'copy-value');
  await click(f.app, 'Copy');
  await focus();
  assert.equal(named(f.app, 'copy-value'), secret);
  assert.equal(secret.value, 'synthetic-created-key');
  assert.deepEqual(f.clipboard, ['synthetic-created-key']);
  window.dispatchEvent(new Event('pagehide'));
  assert.equal(secret.value, '');
  assert.doesNotMatch(f.app.textContent, /Save this key now/);
});

test('own rotation retains a committed but pending response, then its unauthenticated disclosure, across focus', async t => {
  const f = await consoleFixture(t, { own: true });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  assert.equal(operation.sent, true);
  await focus();
  assert.equal(operation.signal.aborted, false, 'same-proof invalidation must not cancel the one-time response');
  assert.match(f.app.textContent, /Saving access change/);
  assert.doesNotMatch(f.app.textContent, /Sign in to your router/);
  operation.resolve(reply({ id: 'mine', key: 'synthetic-replacement-admin' }));
  await drain();
  assert.match(f.app.textContent, /Save your new key/);
  const secret = named(f.app, 'copy-value');
  await click(f.app, 'Copy');
  await focus();
  assert.equal(secret.value, 'synthetic-replacement-admin');
  assert.equal(named(f.app, 'copy-value'), secret);
  assert.deepEqual(f.clipboard, ['synthetic-replacement-admin']);
  assert.doesNotMatch(f.app.textContent, /outcome may be unknown/);
  f.state.csrf = 'proof-B';
  await focus();
  assert.equal(secret.value, '');
  assert.match(f.app.textContent, /Sign in to your router/);
});

for (const boundary of ['csrf', 'issuer']) {
  test(`changed ${boundary} clears pending own rotation and suppresses its late key`, async t => {
    const f = await consoleFixture(t, { own: true });
    const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
    await ownForm(f);
    await click(f.app, 'Rotate key');
    f.state[boundary] = boundary === 'csrf' ? 'proof-other-session' : 'https://another.example';
    await focus();
    assert.equal(operation.signal.aborted, true);
    assert.match(f.app.textContent, /outcome may be unknown/);
    operation.resolve(reply({ key: 'synthetic-obsolete-admin' }));
    await drain();
    assert.doesNotMatch(f.app.textContent, /Save your new key/);
    assert.equal(walk(f.app).some(node => node.value === 'synthetic-obsolete-admin'), false);
  });
}

test('lost own-rotation response ends the exception, clears admin content and explains recovery', async t => {
  const f = await consoleFixture(t, { own: true });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  await focus();
  operation.reject(new Error('response lost'));
  await drain();
  assert.match(f.app.textContent, /Sign in to your router/);
  assert.match(f.app.textContent, /local terminal recovery/);
  assert.match(f.app.textContent, /outcome may be unknown/);
  assert.doesNotMatch(f.app.textContent, /Keys & teams/);
  await focus();
  assert.match(f.app.textContent, /Sign in to your router/);
});

test('a failed probe preserves a provider form but blocks changes until a successful probe', async t => {
  const f = await consoleFixture(t);
  await click(f.app, 'Providers');
  const candidate = named(f.app, 'token');
  candidate.value = 'synthetic-candidate';
  f.failProbe(true);
  await focus();
  assert.equal(candidate.value, 'synthetic-candidate');
  assert.match(f.app.textContent, /New changes are paused/);
  assert.equal(find(f.app, node => node.tag === 'fieldset').inert, true);
  await click(f.app, 'Save credentials');
  assert.equal(f.calls.some(call => call.options.method === 'PUT'), false);
  f.failProbe(false);
  await click(f.app, 'Verify session');
  assert.equal(find(f.app, node => node.tag === 'fieldset').inert, false);
  await click(f.app, 'Save credentials');
  assert.equal(f.calls.filter(call => call.options.method === 'PUT').length, 1);
});

test('a failed focus probe preserves the replacement key; its original earliest expiry still scrubs it', async t => {
  const now = Date.parse('2030-01-01T00:00:00Z');
  const timers = [];
  t.mock.method(Date, 'now', () => now);
  t.mock.method(globalThis, 'setTimeout', (fn, delay) => { timers.push({ fn, delay }); return timers.length; });
  t.mock.method(globalThis, 'clearTimeout', () => {});
  const f = await consoleFixture(t, { own: true, expiresAt: new Date(now + 120000).toISOString(), identityExpiry: new Date(now + 60000).toISOString() });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  operation.resolve(reply({ key: 'synthetic-replacement-admin' }));
  await drain();
  const secret = named(f.app, 'copy-value');
  f.failProbe(true);
  await focus();
  assert.equal(secret.value, 'synthetic-replacement-admin');
  await click(f.app, 'Copy');
  assert.deepEqual(f.clipboard, ['synthetic-replacement-admin']);
  assert.equal(timers.at(-1).delay, 60000);
  timers.at(-1).fn();
  assert.equal(secret.value, '', 'known expiry clears disclosure before any network response');
  await drain();
  assert.doesNotMatch(f.app.textContent, /Save your new key/);
});

test('an obsolete focus probe cannot modify a newly installed identity', async t => {
  const f = await consoleFixture(t);
  const probe = f.hold('GET', '/session');
  await focus();
  assert.equal(probe.sent, true);
  f.unhold('GET', '/session');
  f.state.csrf = 'proof-B';
  f.identity({ id: 'new-user', name: 'New user', role: 'user', is_virtual: true, limits: {}, team: null, usage: null });
  f.authChanged();
  await drain();
  assert.match(f.app.textContent, /My Access/);
  probe.resolve(reply({ authenticated: false, csrf: 'proof-A', issuer: origin }));
  await drain();
  assert.match(f.app.textContent, /My Access/);
  assert.doesNotMatch(f.app.textContent, /Sign in to your router|New changes are paused|Operator/);
});

test('pending create disables and guards Close, editor replacement, tabs, sidebar and sign-out, with unload warning', async t => {
  const f = await consoleFixture(t);
  const operation = f.hold('POST', '/keys');
  await createForm(f);
  const id = named(f.app, 'id');
  await submit(f.app);
  for (const label of ['Close', 'Create key', 'Teams', 'Providers', 'Sign out']) {
    const control = button(f.app, label);
    assert.equal(control.disabled, true, label);
    // Calling the listener bypasses disabled, proving the handler guard too.
    control.listeners.get('click')({ currentTarget: control });
    await drain();
    assert.equal(named(f.app, 'id'), id, `${label} must keep the operation owner`);
  }
  const unload = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(unload);
  assert.equal(unload.defaultPrevented, true);
  assert.equal(operation.signal.aborted, false);
  operation.resolve(reply({ key: 'synthetic-created-key' }));
  await drain();
  assert.match(f.app.textContent, /Save this key now/);
  const settledUnload = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(settledUnload);
  assert.equal(settledUnload.defaultPrevented, false);
});

test('pending provider save blocks provider replacement and reload; diagnostic Test remains cancelable', async t => {
  const f = await consoleFixture(t);
  await click(f.app, 'Providers');
  const candidate = named(f.app, 'token');
  candidate.value = 'synthetic-candidate';
  const save = f.hold('PUT', '/providers/openai/connection');
  await click(f.app, 'Save credentials');
  assert.equal(find(f.app, node => node.dataset.provider === 'groq').disabled, true);
  assert.equal(button(f.app, 'Reload connection').disabled, true);
  await provider(f, 'groq');
  await click(f.app, 'Reload connection');
  assert.equal(named(f.app, 'token'), candidate);
  assert.equal(save.signal.aborted, false);
  save.resolve(reply({ saved: true, durable: true, active: true, published: true }));
  await drain();
  await click(f.app, 'Reload connection');
  named(f.app, 'token').value = 'synthetic-test-candidate';
  const diagnostic = f.hold('POST', '/providers/openai/connection/test');
  await click(f.app, 'Test candidate');
  const unload = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(unload);
  assert.equal(unload.defaultPrevented, false);
  await provider(f, 'groq');
  assert.equal(diagnostic.signal.aborted, true);
  assert.doesNotMatch(f.app.textContent, /outcome may be unknown/);
});

test('cross-tab auth reset retains only a generic interrupted-change notice through login to another identity', async t => {
  const f = await consoleFixture(t);
  const operation = f.hold('POST', '/keys');
  await createForm(f);
  await submit(f.app);
  f.state.authenticated = false;
  f.state.csrf = 'proof-logged-out';
  f.authChanged();
  assert.doesNotMatch(f.app.textContent, /Created key/);
  assert.match(f.app.textContent, /outcome may be unknown/);
  await drain();
  assert.equal(operation.signal.aborted, true);
  assert.match(f.app.textContent, /Sign in to your router/);
  operation.resolve(reply({ key: 'synthetic-lost-value' }));
  await drain();
  f.identity({ id: 'new-user', name: 'New user', role: 'user', is_virtual: true, limits: {}, team: null, usage: null });
  named(f.app, 'key').value = 'synthetic-new-login';
  await submit(f.app);
  assert.match(f.app.textContent, /My Access/);
  assert.match(f.app.textContent, /outcome may be unknown/);
  assert.doesNotMatch(f.app.textContent, /Created key|synthetic-lost-value|Operator/);
  await focus();
  assert.match(f.app.textContent, /outcome may be unknown/);
});

for (const kind of ['create', 'rotate', 'own rotation']) {
  test(`a ${kind} response waits for an overlapping focus probe before disclosing its key`, async t => {
    const own = kind === 'own rotation';
    const f = await consoleFixture(t, { own, managed: kind === 'rotate' });
    const operation = f.hold('POST', kind === 'create' ? '/keys' : '/keys/mine/rotate', () => { if (own) f.state.authenticated = false; });
    if (kind === 'create') { await createForm(f); await submit(f.app); }
    else { await ownForm(f); await click(f.app, 'Rotate key'); }
    const probe = f.hold('GET', '/session');
    await focus();
    operation.resolve(reply({ key: 'synthetic-awaiting-verification' }));
    await drain();
    assert.equal(walk(f.app).some(node => node.value === 'synthetic-awaiting-verification'), false, 'do not reveal while authority is being observed');
    assert.equal(button(f.app, 'Close').disabled, true, 'keep owner/navigation guarded after response but before verification');
    f.unhold('GET', '/session');
    probe.resolve(reply({ ...f.state }));
    await drain();
    assert.equal(named(f.app, 'copy-value').value, 'synthetic-awaiting-verification');
    assert.doesNotMatch(f.app.textContent, /outcome may be unknown/);
  });
}

test('response-first own rotation cannot discard an overlapping changed-identity probe', async t => {
  const f = await consoleFixture(t, { own: true });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  const probe = f.hold('GET', '/session');
  await focus();
  operation.resolve(reply({ key: 'synthetic-prior-identity-admin' }));
  await drain();
  f.state.csrf = 'proof-B';
  f.state.authenticated = true;
  f.identity({ id: 'new-user', name: 'New user', role: 'user', is_virtual: true, limits: {}, team: null, usage: null });
  f.unhold('GET', '/session');
  const reads = f.calls.filter(call => call.path.endsWith('/session')).length;
  probe.resolve(reply({ ...f.state }));
  await drain();
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-prior-identity-admin'), false);
  assert.match(f.app.textContent, /My Access/);
  assert.match(f.app.textContent, /outcome may be unknown/);
  assert.match(f.app.textContent, /another administrator key or local terminal recovery/);
  assert.equal(f.calls.filter(call => call.path.endsWith('/session')).length, reads + 2, 'the changed-anchor refresh bootstraps exactly twice');
});

for (const retry of ['same anchor', 'changed anchor', 'auth reset']) {
  test(`failed overlapping probe keeps a response hidden through ${retry}`, async t => {
    const f = await consoleFixture(t, { own: true });
    const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
    await ownForm(f);
    await click(f.app, 'Rotate key');
    const probe = f.hold('GET', '/session');
    await focus();
    operation.resolve(reply({ key: 'synthetic-held-replacement' }));
    await drain();
    probe.reject(new Error('read interrupted'));
    await drain();
    assert.equal(walk(f.app).some(node => node.value === 'synthetic-held-replacement'), false);
    assert.equal(button(f.app, 'Close').disabled, true);
    assert.match(f.app.textContent, /New changes are paused/);
    const sentWrites = f.calls.filter(call => call.options.method === 'POST').length;
    f.unhold('GET', '/session');
    if (retry !== 'same anchor') f.state.csrf = 'proof-B';
    if (retry === 'auth reset') { f.authChanged(); await drain(); }
    else await click(f.app, 'Verify session');
    assert.equal(f.calls.filter(call => call.options.method === 'POST').length, sentWrites, 'verification never retries the write');
    if (retry === 'same anchor') assert.equal(named(f.app, 'copy-value').value, 'synthetic-held-replacement');
    else {
      assert.equal(walk(f.app).some(node => node.value === 'synthetic-held-replacement'), false);
      assert.match(f.app.textContent, /Sign in to your router/);
      assert.match(f.app.textContent, /outcome may be unknown/);
      assert.match(f.app.textContent, /another administrator key or local terminal recovery/);
    }
  });
}

test('an older matching probe cannot release a key while newer authority is pending', async t => {
  const f = await consoleFixture(t, { own: true });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  const older = f.hold('GET', '/session');
  await focus();
  f.unhold('GET', '/session');
  const newer = f.hold('GET', '/session');
  await focus();
  operation.resolve(reply({ key: 'synthetic-pending-latest-probe' }));
  await drain();
  older.resolve(reply({ ...f.state }));
  await drain();
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-pending-latest-probe'), false);
  assert.equal(button(f.app, 'Close').disabled, true, 'the newer authority check still owns verification');
  f.state.csrf = 'proof-B';
  f.unhold('GET', '/session');
  newer.resolve(reply({ ...f.state }));
  await drain();
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-pending-latest-probe'), false);
  assert.match(f.app.textContent, /Sign in to your router/);
});

test('expiry aborts a received key waiting after probe failure', async t => {
  const now = Date.parse('2030-01-01T00:00:00Z');
  const timers = [];
  t.mock.method(Date, 'now', () => now);
  t.mock.method(globalThis, 'setTimeout', (fn, delay) => { timers.push({ fn, delay }); return timers.length; });
  t.mock.method(globalThis, 'clearTimeout', () => {});
  const f = await consoleFixture(t, { own: true, expiresAt: new Date(now + 120000).toISOString(), identityExpiry: new Date(now + 60000).toISOString() });
  const expiry = timers.at(-1);
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  const probe = f.hold('GET', '/session');
  await focus();
  operation.resolve(reply({ key: 'synthetic-expiring-hidden-key' }));
  await drain();
  probe.reject(new Error('probe failed'));
  await drain();
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-expiring-hidden-key'), false);
  assert.equal(button(f.app, 'Close').disabled, true);
  assert.equal(expiry.delay, 60000, 'original earliest expiry remains scheduled during verification');
  f.unhold('GET', '/session');
  const reads = f.calls.filter(call => call.path.endsWith('/session')).length;
  expiry.fn();
  await drain();
  assert.equal(operation.signal.aborted, true);
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-expiring-hidden-key'), false);
  assert.match(f.app.textContent, /Sign in to your router/);
  assert.match(f.app.textContent, /outcome may be unknown/);
  assert.match(f.app.textContent, /another administrator key or local terminal recovery/);
  assert.equal(f.calls.filter(call => call.path.endsWith('/session')).length, reads + 1, 'guidance does not start another expiry refresh');
  const unload = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(unload);
  assert.equal(unload.defaultPrevented, false, 'expiry retires the pending result owner');
  await focus();
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-expiring-hidden-key'), false);
});

test('lost own-rotation guidance survives failed recovery and retries until successful sign-in', async t => {
  const f = await consoleFixture(t, { own: true });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  f.failProbe(true);
  operation.reject(new Error('rotation response lost'));
  await drain();
  assert.match(f.app.textContent, /Connection needs attention/);
  assert.match(f.app.textContent, /replacement key was not received/);
  assert.match(f.app.textContent, /another administrator key or local terminal recovery/);
  await click(f.app, 'Refresh session');
  assert.match(f.app.textContent, /local terminal recovery/);
  f.failProbe(false);
  await click(f.app, 'Refresh session');
  assert.match(f.app.textContent, /Sign in to your router/);
  assert.match(f.app.textContent, /local terminal recovery/);
  named(f.app, 'key').value = 'synthetic-recovered-administrator';
  await submit(f.app);
  assert.equal(button(f.app, 'Sign out').disabled, false);
  assert.doesNotMatch(f.app.textContent, /replacement key was not received|another administrator key or local terminal recovery/);
});

test('explicit sign-out works after a failed focus probe and clears protected values before sending', async t => {
  const f = await consoleFixture(t);
  await click(f.app, 'Providers');
  const candidate = named(f.app, 'token');
  candidate.value = 'synthetic-signout-candidate';
  f.failProbe(true);
  await focus();
  assert.match(f.app.textContent, /New changes are paused/);
  assert.equal(button(f.app, 'Sign out').disabled, false);
  f.failProbe(false);
  const logout = f.hold('DELETE', '/session', () => {
    assert.equal(candidate.value, '', 'explicit logout must clear the old form before dispatch');
    f.state.authenticated = false;
    f.state.csrf = 'proof-logged-out';
  });
  await click(f.app, 'Sign out');
  assert.equal(logout.sent, true, 'the enabled sign-out control must issue a logout request');
  assert.equal(candidate.value, '');
  logout.resolve(reply({}));
  await drain();
  assert.match(f.app.textContent, /Sign in to your router/);
  assert.equal(f.calls.filter(call => call.options.method === 'DELETE').length, 1);
});

test('pagehide preserves recovery guidance for a withheld own-rotation result without a refresh', async t => {
  const f = await consoleFixture(t, { own: true });
  const operation = f.hold('POST', '/keys/mine/rotate', () => { f.state.authenticated = false; });
  await ownForm(f);
  await click(f.app, 'Rotate key');
  const probe = f.hold('GET', '/session');
  await focus();
  operation.resolve(reply({ key: 'synthetic-pagehide-replacement' }));
  await drain();
  const requests = f.calls.length;
  window.dispatchEvent(new Event('pagehide'));
  await drain();
  assert.match(f.app.textContent, /another administrator key or local terminal recovery/);
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-pagehide-replacement'), false);
  assert.equal(f.calls.length, requests, 'recording the guidance must not refresh or replay');
  probe.resolve(reply({ ...f.state }));
  await drain();
  assert.equal(walk(f.app).some(node => node.value === 'synthetic-pagehide-replacement'), false);
});

for (const kind of ['create', 'rotate another key']) {
  test(`an aborted ${kind} wait never claims the caller lost its own replacement key`, async t => {
    const f = await consoleFixture(t, { managed: kind !== 'create' });
    const operation = f.hold('POST', kind === 'create' ? '/keys' : '/keys/mine/rotate');
    if (kind === 'create') { await createForm(f); await submit(f.app); }
    else { await ownForm(f); await click(f.app, 'Rotate key'); }
    const probe = f.hold('GET', '/session');
    await focus();
    operation.resolve(reply({ key: 'synthetic-unrelated-key' }));
    await drain();
    f.state.authenticated = false;
    f.state.csrf = 'proof-other-session';
    f.unhold('GET', '/session');
    const requests = f.calls.length;
    f.authChanged();
    await drain();
    assert.match(f.app.textContent, /Sign in to your router/);
    assert.match(f.app.textContent, /outcome may be unknown/);
    assert.doesNotMatch(f.app.textContent, /another administrator key or local terminal recovery/);
    assert.equal(f.calls.length, requests + 1, 'only the auth-state refresh may send another request');
    probe.resolve(reply({ ...f.state }));
    await drain();
    assert.equal(walk(f.app).some(node => node.value === 'synthetic-unrelated-key'), false);
  });
}
