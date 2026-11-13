import test from 'node:test';
import assert from 'node:assert/strict';
import { access } from '../access.mjs';

import { Element, find, named, button, drain, installDOM } from './dom-fixture.mjs';

const now = Date.parse('2030-01-01T00:00:00Z');
const iso = milliseconds => new Date(now + milliseconds).toISOString();
for (const [label, sessionExpiry, keyExpiry, expected] of [
  ['earlier managed-key expiry', iso(12 * 3600000), iso(60000), 60000],
  ['earlier session expiry', iso(30000), iso(60000), 30000],
  ['missing key expiry', iso(60000), undefined, 60000],
  ['invalid session with valid key expiry', 'invalid', iso(60000), 60000],
]) {
  test(`actual console schedules and clears protected content at ${label}`, async t => {
    const app = installDOM(t);
    const timers = [];
    let expired = false;
    t.mock.method(Date, 'now', () => now);
    t.mock.method(globalThis, 'setTimeout', (fn, delay) => { timers.push({ fn, delay }); return timers.length; });
    t.mock.method(globalThis, 'clearTimeout', () => {});
    t.mock.method(globalThis, 'fetch', async path => ({ ok: true, status: 200, json: async () => {
      if (path.endsWith('/session')) return { data: { authenticated: !expired, csrf: 'fixture-proof', issuer: location.origin, node_name: 'Fixture', expires_at: sessionExpiry } };
      if (path.endsWith('/me')) return { data: { id: 'fixture', name: 'Fixture', role: 'user', is_virtual: true, expires_at: keyExpiry, limits: {}, team: null, usage: null } };
      if (path.endsWith('/me/connections')) return { data: [] };
      assert.fail(`unexpected request: ${path}`);
    } }));
    await import(`../console.mjs?review=${encodeURIComponent(label)}`);
    await drain();
    assert.equal(timers.length, 1);
    assert.equal(timers[0].delay, expected);
    assert.match(app.textContent, /My Access/);
    const secret = new Element('input');
    secret.type = 'password';
    secret.value = 'synthetic-transient-value';
    app.append(secret);
    expired = true;
    timers[0].fn();
    assert.equal(secret.value, '', 'expiry clears a transient secret before the refresh completes');
    assert.doesNotMatch(app.textContent, /My Access/);
    await drain();
    assert.match(app.textContent, /Sign in to your router/);
  });
}

async function editor(t, { identity, record, uncertain = false }) {
  const root = installDOM(t);
  const calls = [];
  let refreshes = 0;
  let ended = 0;
  const p = {
    root, identity, current: () => true, scope: { signal: new AbortController().signal }, go() {},
    refresh: async () => { refreshes++; }, showKeyAfterSession: () => { ended++; },
    beginOwnRotation: () => ({}), endOwnRotation() {}, syncActivity() {},
    api: {
      canLeave: () => true, canChange: () => true,
      list: async path => path === '/keys' ? [record] : [],
      data: async (path, options = {}) => {
        if (!options.method) return record;
        calls.push({ path, ...options });
        if (uncertain) throw Object.assign(new Error('Response lost'), { uncertain: true });
        return path.endsWith('/rotate') ? { id: record.id, key: 'synthetic-rotated-value' } : { ...record, ...options.body };
      },
    },
  };
  await access(p);
  button(root, 'Manage').listeners.get('click')();
  return { root, calls, refreshes: () => refreshes, ended: () => ended };
}

for (const isVirtual of [false, true]) {
  test(`rotation identifies an admin ID collision with is_virtual=${isVirtual}`, async t => {
    const form = await editor(t, { identity: { id: 'admin', is_virtual: isVirtual }, record: { id: 'admin', name: 'Managed administrator', role: 'admin' } });
    await button(form.root, 'Rotate key').listeners.get('click')();
    assert.equal(form.calls.length, 1);
    assert.equal(form.ended(), isVirtual ? 1 : 0);
    assert.equal(form.refreshes(), 0);
    if (!isVirtual) assert.match(form.root.textContent, /Save this key now/);
  });
}

for (const uncertain of [false, true]) {
  test(`editing own expiry rechecks identity and its timer, uncertain=${uncertain}`, async t => {
    const form = await editor(t, { identity: { id: 'mine', is_virtual: true }, record: { id: 'mine', name: 'My key', role: 'admin' }, uncertain });
    named(form.root, 'expires_at').value = '2030-01-01T00:01';
    find(form.root, node => node.tag === 'form').listeners.get('submit')({ preventDefault() {} });
    await drain();
    assert.equal(form.calls.length, 1);
    assert.equal(form.calls[0].method, 'PATCH');
    assert.deepEqual(form.calls[0].body, { expires_at: new Date('2030-01-01T00:01').toISOString() });
    assert.equal(form.refreshes(), 1, 'a current-key expiry change must replace the old session timer');
    assert.equal(form.ended(), 0);
  });
}

test('a fine-grained spend limit remains valid and untouched during a rename', async t => {
  const form = await editor(t, { identity: { id: 'startup-admin', is_virtual: false }, record: { id: 'other', name: 'Before', role: 'user', spend_limit: 0.00005 } });
  const spend = named(form.root, 'spend_limit');
  assert.equal(spend.getAttribute('step'), 'any', 'the input must accept the API numeric precision');
  assert.equal(Number(spend.value), 0.00005);
  named(form.root, 'name').value = 'After';
  find(form.root, node => node.tag === 'form').listeners.get('submit')({ preventDefault() {} });
  await drain();
  assert.equal(form.calls.length, 1);
  assert.deepEqual(form.calls[0].body, { name: 'After' });
});
