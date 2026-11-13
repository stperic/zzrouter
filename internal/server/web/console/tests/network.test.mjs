import test from 'node:test';
import assert from 'node:assert/strict';
import { details } from '../details.mjs';
import { input } from '../dom.mjs';
import { overview } from '../overview.mjs';
import { showNode, addNode } from '../nodes.mjs';
import { clientKey, nodeSummary, networkNodes, cloudProviders } from '../network-data.mjs';
import { walk, button, named, find, drain, installDOM } from './dom-fixture.mjs';

const ok = value => ({ status: 'fulfilled', value });
const failed = { status: 'rejected', reason: new Error('inventory unavailable') };
const node = { name: 'atlas', health_status: 'healthy', os: 'linux', providers: [{ key: 'ollama', name: 'Ollama', kind: 'external', running: true }] };
const provider = { name: 'ollama', node: 'atlas', kind: 'external', enabled: true, installed: true };
const inventory = [ok([node, { name: 'spruce', health_status: 'down' }]), ok([provider])];
const client = { key_id: 'admin', name: 'Editor', source: 'managed', protocol: 'inference-mcp', authentication: 'oauth', active_requests: 0, request_count: 5, last_seen_at: '2026-01-01T00:00:00Z' };
const submit = async root => { find(root, element => element.tag === 'form').dispatchEvent(new Event('submit', { cancelable: true })); await drain(); };

function fixture(t) {
  const root = installDOM(t);
  const controller = new AbortController();
  t.after(() => controller.abort());
  const routes = [];
  const writes = [];
  const api = {
    canLeave: () => true, canChange: () => true,
    list: async path => {
      if (path === '/nodes') return inventory[0].value;
      if (path === '/providers' || path === '/providers?node=atlas') return inventory[1].value;
      if (path === '/me/connections') return [{ id: 'one' }, { id: 'two' }];
      if (path.startsWith('/providers/catalog')) return [{ key: 'groq', name: 'Groq', kind: 'cloud', available: true }];
      assert.fail(path);
    },
    request: async (path, options = {}) => {
      if (path === '/zzrouter/v1/clients') return { data: [client], metadata: {} };
      writes.push({ path, ...options });
      return { node_name: 'new-node' };
    },
    data: async (path, options) => { writes.push({ path, ...options }); return {}; },
  };
  return { root, controller, routes, writes, p: { root, api, state: { node_name: 'atlas' }, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, syncActivity() {}, go: (...route) => routes.push(route) } };
}

test('physical node counts deduplicate identity and preserve unknown failures', () => {
  const duplicated = [ok([node, node, { name: 'spruce', health_status: 'down' }]), inventory[1]];
  assert.deepEqual(nodeSummary(duplicated), { total: 2, healthy: 1, down: 1, unknown: 0, degraded: 0 });
  assert.equal(nodeSummary([failed, inventory[1]]), null);
  assert.deepEqual(networkNodes([failed, inventory[1]]), [{ name: 'atlas' }]);
  assert.notEqual(clientKey(client), clientKey({ ...client, source: 'startup' }));
  assert.notEqual(clientKey(client), clientKey({ ...client, authentication: 'api-key' }));
});

test('cloud providers associate reported nodes without conflating registry or local runtimes', () => {
  const result = cloudProviders(ok([
    { key: 'groq', name: 'Groq', kind: 'cloud', available: true },
    { key: 'huggingface', kind: 'registry', available: true },
    { key: 'ollama', kind: 'external', available: true },
  ]), [inventory[0], ok([provider, { node: 'atlas', name: 'groq', kind: 'cloud', enabled: true }])]);
  assert.deepEqual(result, [{ key: 'groq', name: 'Groq', configured: true, nodes: ['atlas'] }]);
});

test('overview duplicates the coordinator role visually, not in totals, and all cards navigate', async t => {
  const f = fixture(t);
  await overview(f.p);
  const coordinatorCards = walk(f.root).filter(element => element.dataset.node === 'atlas');
  assert.equal(coordinatorCards.length, 2);
  assert.match(f.root.textContent, /1 \/ 2 compute nodes healthy/);
  for (const card of coordinatorCards) card.dispatchEvent(new Event('click'));
  assert.deepEqual(f.routes, [['overview', 'node/atlas'], ['overview', 'node/atlas']]);
  assert.match(f.root.textContent, /Coordinator \+ Worker/);
  assert.match(f.root.textContent, /Key: EditorLast active/);
  assert.match(f.root.textContent, /0 active identities at refresh · 1 observed/);
  assert.ok(button(f.root, '+ Connect client'));
  assert.doesNotMatch(f.root.textContent, /2 connected|Online|null/);
  const cloud = walk(f.root).find(element => element.tag === 'button' && element.textContent.startsWith('Groq'));
  cloud.dispatchEvent(new Event('click'));
  assert.deepEqual(f.routes.at(-1), ['overview', 'cloud/groq']);
});

test('late overview reads cannot restore a closed page', async t => {
  const f = fixture(t);
  let resolve;
  const original = f.p.api.list;
  f.p.api.list = path => path === '/nodes' ? new Promise(done => { resolve = done; }) : original(path);
  const pending = overview(f.p);
  f.controller.abort();
  f.root.textContent = 'Next page';
  resolve([node]);
  await pending;
  assert.equal(f.root.textContent, 'Next page');
});

test('node edits recheck and patch only the selected node', async t => {
  const f = fixture(t);
  showNode(f.p, 'atlas', inventory);
  button(f.root, 'Edit').dispatchEvent(new Event('click'));
  const checkbox = named(f.root, 'ollama');
  checkbox.checked = false;
  checkbox.dispatchEvent(new Event('change'));
  await submit(f.root);
  assert.equal(f.writes.length, 1);
  assert.equal(f.writes[0].path, '/providers/ollama?node=atlas');
  assert.deepEqual(f.writes[0].body, { enabled: false });
  assert.match(f.root.textContent, /Provider settings saved/);
  assert.equal(walk(f.root).some(node => node.tag === 'form'), false);
  assert.equal(f.writes.length, 1, 'no replay after completion');
});

test('node editor refuses stale settings and avoids duplicate submission during recheck', async t => {
  const f = fixture(t);
  let resolve;
  let reads = 0;
  f.p.api.list = () => { reads++; return new Promise(done => { resolve = done; }); };
  showNode(f.p, 'atlas', inventory);
  button(f.root, 'Edit').dispatchEvent(new Event('click'));
  const checkbox = named(f.root, 'ollama');
  checkbox.checked = false;
  checkbox.dispatchEvent(new Event('change'));
  await submit(f.root);
  await submit(f.root);
  assert.equal(reads, 1);
  resolve([{ ...provider, enabled: false }]);
  await drain();
  assert.equal(f.writes.length, 0);
  assert.match(f.root.textContent, /settings changed/);
});

function pairingForm(f) {
  addNode(f.p);
  named(f.root, 'worker_url').value = 'https://new-node.example:9091';
  named(f.root, 'worker_fingerprint').value = 'synthetic-fingerprint';
  named(f.root, 'pairing_code').value = 'synthetic-code';
}

test('pairing consumes the bare response and distinguishes enrollment from healthy observation', async t => {
  const f = fixture(t);
  pairingForm(f);
  await submit(f.root);
  assert.equal(f.writes.length, 1);
  assert.equal(f.writes[0].timeoutMs, 60000);
  assert.equal(f.writes[0].path, '/zzrouter/v1/cluster/pairing/accept');
  assert.equal(named(f.root, 'pairing_code').value, '');
  assert.match(f.root.textContent, /new-node is paired. Healthy node observation is still pending/);
  await submit(f.root);
  assert.equal(f.writes.length, 1);
});

test('pairing error after commitment clears code, blocks replay and directs inspection', async t => {
  const f = fixture(t);
  f.p.api.request = async () => { f.writes.push('attempt'); throw Object.assign(new Error('Certificate confirmation failed'), { status: 400 }); };
  pairingForm(f);
  await submit(f.root);
  assert.equal(named(f.root, 'pairing_code').value, '');
  assert.match(f.root.textContent, /Enrollment may already have completed/);
  assert.ok(button(f.root, 'Inspect nodes'));
  await submit(f.root);
  assert.equal(f.writes.length, 1);
});

test('pairing rejects untrusted URL forms and abort clears a transient code', async t => {
  const f = fixture(t);
  pairingForm(f);
  named(f.root, 'worker_url').value = 'https://name:secret@new-node.example';
  await submit(f.root);
  assert.equal(f.writes.length, 0);
  f.controller.abort();
  assert.equal(named(f.root, 'pairing_code').value, '');
});

test('confirmed pairing survives an unavailable health readback', async t => {
 const f = fixture(t);
 f.p.api.list = async () => { throw new Error('Node inventory unavailable'); };
 pairingForm(f);
 await submit(f.root);
 assert.match(f.root.textContent, /new-node is paired. Current node observations are unavailable/);
 assert.doesNotMatch(f.root.textContent, /Pairing was not confirmed/);
});

test('automatic refresh preserves a control focused while inventory is in flight', async t => {
 const f = fixture(t);
 let poll;
 t.mock.method(globalThis, 'setTimeout', fn => { poll = fn; return { unref() {} }; });
 t.mock.method(globalThis, 'clearTimeout', () => {});
 await overview(f.p);
 const original = f.p.api.list;
 let release;
 f.p.api.list = path => path === '/nodes' ? new Promise(done => { release = done; }) : original(path);
 const oldCard = find(f.root, element => element.dataset.node === 'spruce');
 const refresh = poll();
 oldCard.focus();
 release([{ ...node, name: 'changed-node' }]);
 await refresh;
 assert.ok(f.root.contains(oldCard), 'late automatic response preserves focus and the displayed snapshot');
 assert.doesNotMatch(f.root.textContent, /changed-node/);
 document.activeElement = undefined;
 f.p.api.list = path => path === '/nodes' ? Promise.resolve([{ ...node, name: 'changed-node' }]) : original(path);
 await poll();
 assert.match(f.root.textContent, /changed-node/);
});

test('unavailable node reads never claim a configured cloud provider has no enabled node', async t => {
 const f = fixture(t);
 const original = f.p.api.list;
 f.p.api.list = path => ['/nodes', '/providers'].includes(path) ? Promise.reject(new Error('unavailable')) : original(path);
 await overview(f.p, 'cloud/groq');
 assert.match(f.root.textContent, /Groq.*ConfigurationConfiguredReported onNode enablement unavailable/s);
 assert.doesNotMatch(f.root.textContent, /No node reports this provider enabled/);
});

test('detail dialogs intercept Escape during writes and scrub secrets on owner abort', t => {
 const f = fixture(t);
 let canLeave = false;
 let closed = 0;
 f.p.api.canLeave = () => canLeave;
 const owner = details(f.p, 'Node details', () => { closed++; });
 const dialog = find(f.root, element => element.tag === 'dialog');
 assert.equal(dialog.getAttribute('open'), '');
 const secret = input('code', 'synthetic-code', { type: 'password' });
 owner.root.append(secret);
 const cancel = new Event('cancel', { cancelable: true });
 dialog.dispatchEvent(cancel);
 assert.equal(cancel.defaultPrevented, true);
 assert.equal(closed, 0);
 canLeave = true;
 dialog.dispatchEvent(new Event('cancel', { cancelable: true }));
 assert.equal(closed, 1);
 f.controller.abort();
 assert.equal(dialog.getAttribute('open'), undefined);
 assert.equal(secret.value, '');
 assert.equal(owner.current(), false);
});
