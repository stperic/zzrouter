import test from 'node:test';
import assert from 'node:assert/strict';
import { connectionGuide } from '../clients.mjs';
import { membershipSummary } from '../access-form.mjs';
import { auditHistory } from '../audit.mjs';
import { recovery } from '../recovery.mjs';
import { installDOM, button, drain, walk } from './dom-fixture.mjs';

function owner(t, data = async () => ({})) {
  const root = installDOM(t), controller = new AbortController();
  t.after(() => controller.abort());
  return { root, state: { issuer: 'http://localhost:9090', sharing: 'local_only' }, identity: { role: 'admin' }, scope: { signal: controller.signal }, current: () => !controller.signal.aborted,
    api: { data, canLeave: () => true }, go() {} };
}

test('client guide separates browser sign-in and application keys with local sharing guidance', t => {
  const p = owner(t);
  p.root.append(connectionGuide(p));
  assert.match(p.root.textContent, /This computer only/);
  assert.match(p.root.textContent, /No app key is needed/);
  assert.match(p.root.textContent, /API key for an app or automation/);
  assert.match(p.root.textContent, /refresh Clients/);
  assert.match(p.root.textContent, /Administrator account authorizes management MCP/);
  assert.ok(walk(p.root).some(node => String(node.value).endsWith('/zzrouter/v1/mcp')));
  p.identity.role = 'user';
  p.root.replaceChildren(connectionGuide(p));
  assert.doesNotMatch(p.root.textContent, /refresh Clients/);
  assert.doesNotMatch(p.root.textContent, /Management MCP endpoint/);
  assert.match(p.root.textContent, /Use a User account for inference/);
  assert.match(p.root.textContent, /Your administrator can confirm/);
});

test('membership uses the backend summary and keeps missing observations unknown', () => {
  assert.equal(membershipSummary({}), 'Membership unavailable');
  assert.equal(membershipSummary({ membership: { people: 2, app_keys: 1, invitations: 3, enrollment_grants: 1 } }), '2 people · 1 app key · 3 pending invitations · 1 other pending enrollment');
});

test('access history loads older pages and presents stale cursor recovery', async t => {
  const calls = [];
  const p = owner(t, async path => {
    calls.push(path);
    if (calls.length === 3) throw Object.assign(new Error('History changed'), { status: 409 });
    return { events: [{ time: '2026-11-16T00:00:00Z', action: 'person.created', actor: 'person-safe', subject: 'person-safe' }], next: 'opaque cursor', skipped: 0 };
  });
  await auditHistory(p);
  await button(p.root, 'Load older').listeners.get('click')(); await drain();
  assert.match(calls[1], /cursor=opaque%20cursor/);
  assert.match(p.root.textContent, /2 events shown/);
  await button(p.root, 'Load older').listeners.get('click')(); await drain();
  assert.match(p.root.textContent, /Refresh to read the current audit file/);
});

test('recovery shows server-owned paths and staged-only commands', async t => {
  const p = owner(t, async path => {
    assert.equal(path, '/system/recovery');
    return { node: 'coordinator', platform: 'darwin', release: 'test', config_dir: "/Library/Test's router", separate: [{ reason: 'Model weights require separate recovery', path: '/models' }] };
  });
  await recovery(p);
  assert.ok(walk(p.root).some(node => String(node.value).includes('recovery backup')));
  assert.match(p.root.textContent, /It never replaces a running installation/);
  assert.match(p.root.textContent, /Model weights require separate recovery/);
});
