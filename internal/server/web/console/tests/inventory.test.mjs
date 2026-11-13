import test from 'node:test';
import assert from 'node:assert/strict';
import { API } from '../api.mjs';
import { readInventory, nodeInventory, cloudInventory, providerCategory } from '../inventory.mjs';
import { find, walk, installDOM } from './dom-fixture.mjs';

const fulfilled = value => ({ status: 'fulfilled', value });
const rejected = message => ({ status: 'rejected', reason: new Error(message) });
const local = { key: 'engine-a', name: 'engine-a', kind: 'on-demand', cloud: false };
const nodes = [
  { name: 'atlas', cluster_role: 'coordinator', health_status: 'healthy', os: 'synthetic', version: '1', providers: [local] },
  { name: 'spruce', cluster_role: 'worker', health_status: 'down', last_seen_at: '2030-01-01T12:00:00Z', providers: [local], last_error: 'dial 10.9.8.7 refused', address: '10.9.8.7' },
  { name: 'pine', cluster_role: 'worker', health_status: 'degraded', providers: [local] },
  { name: 'cedar', cluster_role: 'worker', health_status: 'unknown' },
];
const record = (name, node, enabled, installed, kind = 'on-demand') => ({ name, node, enabled, installed, kind, mode: kind, managed: installed, version: installed ? '1' : '', endpoint: 'http://10.9.8.7:9999' });
const records = [
  record('engine-a', 'atlas', true, true), record('engine-a', 'spruce', true, true),
  record('engine-a', 'pine', true, true),
  record('engine-b', 'atlas', false, true), record('remote-x', 'spruce', true, false, 'cloud'),
];
const page = () => ({ api: { canLeave: () => true }, go() {}, state: { node_name: 'atlas' } });
const nodeCard = (root, name) => find(root, node => node.tag === 'h2' && node.textContent === name).parent;

test('inventory reads reuse API pagination and preserve independent failures', async () => {
  const calls = [];
  const api = new API({ origin: 'https://router.example', fetcher: async path => {
    calls.push(path);
    if (path.endsWith('/nodes')) throw new Error('nodes unavailable');
    const value = path.includes('offset=1') ? { data: records.slice(1), total: records.length, has_more: false } : { data: records.slice(0, 1), total: records.length, has_more: true };
    return { ok: true, status: 200, json: async () => value };
  } });
  const controller = new AbortController();
  const [observedNodes, providers] = await readInventory({ api, scope: { signal: controller.signal } });
  assert.equal(observedNodes.status, 'rejected');
  assert.deepEqual(providers.value, records);
  assert.deepEqual(calls, ['/zzrouter/v1/nodes', '/zzrouter/v1/providers', '/zzrouter/v1/providers?offset=1']);
});

test('node inventory joins provider names within node scope and retains stale and unknown observations', t => {
  installDOM(t);
  const root = nodeInventory(page(), [fulfilled(nodes), fulfilled(records)]);
  const atlas = nodeCard(root, 'atlas');
  assert.match(atlas.textContent, /coordinatorHealthy/);
  assert.match(atlas.textContent, /engine-aView local providers/);
  assert.doesNotMatch(atlas.textContent, /engine-b|remote-x|Disabled|Not installed/);
  const spruce = nodeCard(root, 'spruce');
  assert.match(spruce.textContent, /workerDown/);
  assert.match(spruce.textContent, /Last reported provider state/);
  assert.match(spruce.textContent, /engine-aView local providers/);
  assert.ok(spruce.textContent.includes(new Date(nodes[1].last_seen_at).toLocaleString()), 'Last seen displays the last successful observation');
  assert.match(nodeCard(root, 'pine').textContent, /Degraded/);
  assert.match(nodeCard(root, 'cedar').textContent, /Unknown/);
  assert.match(nodeCard(root, 'cedar').textContent, /Provider status has not been reported by this node/);
  assert.doesNotMatch(root.textContent, /10\.9\.8\.7|Connected|Online/);
  const chips = find(atlas, node => node.tag === 'ul');
  assert.deepEqual(chips.children.map(node => [node.tag, node.textContent]), [['li', 'engine-a']], 'each provider is a separate native list item');
});

test('local details distinguish disabled installed providers from missing installation and routing evidence', t => {
  installDOM(t);
  const dormant = nodeInventory(page(), [fulfilled(nodes), fulfilled(records)], { key: 'engine-b', name: 'engine-b' });
  assert.match(nodeCard(dormant, 'atlas').textContent, /engine-bDisabledInstalled/);
  for (const name of ['spruce', 'pine', 'cedar']) assert.ok(nodeCard(dormant, name), 'every known node remains visible');
  const withoutInstall = [{ ...nodes[0], providers: [] }];
  const engine = nodeInventory(page(), [fulfilled(withoutInstall), fulfilled([record('engine-a', 'atlas', false, false)])], local);
  assert.match(nodeCard(engine, 'atlas').textContent, /DisabledNot installed/);
  assert.doesNotMatch(nodeCard(engine, 'atlas').textContent, /failed|broken|not running/i);
});

test('independent inventory snapshots retain flat provider facts before a node report is available', t => {
  installDOM(t);
  // These reads deliberately straddle a successful probe; they are not one atomic snapshot.
  const laterRecords = [...records, record('engine-a', 'cedar', false, false)];
  const root = nodeInventory(page(), [fulfilled(nodes), fulfilled(laterRecords)], local);
  assert.match(nodeCard(root, 'cedar').textContent, /DisabledNot installed/);
  assert.match(nodeCard(root, 'cedar').textContent, /Provider status has not been reported by this node/);
});

test('read failures leave useful node inventory and provider facts without inventing reachability', t => {
  installDOM(t);
  const withoutProviders = nodeInventory(page(), [fulfilled(nodes), rejected('provider read unavailable')]);
  assert.match(withoutProviders.textContent, /provider read unavailable/);
  assert.match(nodeCard(withoutProviders, 'atlas').textContent, /engine-a \(enablement unavailable\)/);
  assert.match(nodeCard(withoutProviders, 'cedar').textContent, /Provider status unavailable/);
  const inactive = nodeInventory(page(), [fulfilled(nodes), rejected('provider read unavailable')], { key: 'engine-b', name: 'engine-b' });
  assert.match(nodeCard(inactive, 'atlas').textContent, /Provider status unavailable/);
  assert.doesNotMatch(inactive.textContent, /has not been reported|No enabled local providers/);
  const withoutNodes = nodeInventory(page(), [rejected('node read unavailable'), fulfilled(records)]);
  assert.match(withoutNodes.textContent, /node read unavailable/);
  assert.match(nodeCard(withoutNodes, 'spruce').textContent, /Role unavailableUnavailable/);
  assert.match(nodeCard(withoutNodes, 'spruce').textContent, /Last seenUnavailableOS \/ Router versionUnavailable/);
  assert.doesNotMatch(withoutNodes.textContent, /Not reported|has not been reported|No enabled local providers/);
  assert.match(nodeCard(withoutNodes, 'spruce').textContent, /Last reported provider state/);
});

test('cloud configuration and per-node enablement stay separate from health and catalog installation', t => {
  installDOM(t);
  const catalog = fulfilled([
    { key: 'remote-x', name: 'remote-x', kind: 'cloud', is_cloud: true, available: true, installed: true },
    { key: 'remote-y', name: 'remote-y', kind: 'cloud', is_cloud: true, available: false, installed: true },
    { key: 'huggingface', name: 'huggingface', kind: 'registry', is_cloud: false, available: true },
  ]);
  const root = cloudInventory(page(), catalog, [fulfilled(nodes), fulfilled([...records, record('remote-x', 'atlas', false, false, 'cloud')])]);
  assert.match(root.textContent, /Credential configuration on atlas/);
  assert.match(root.textContent, /1 of 2 cloud services configured; 1 not configured/);
  assert.match(root.textContent, /remote-xConfigured on coordinatorspruce: enabled \(last reported\)/);
  assert.doesNotMatch(root.textContent, /remote-y|atlas: disabled/);
  assert.doesNotMatch(root.textContent, /huggingface|Installed|Connected|10\.9\.8\.7/);
  assert.equal(providerCategory({ kind: 'registry', is_cloud: true }), 'registries');
  assert.equal(providerCategory({ kind: 'cloud', is_cloud: false }), 'cloud');
});

test('catalog and node failures do not erase the other section', t => {
  installDOM(t);
  const inventory = [fulfilled(nodes), fulfilled(records)];
  assert.match(cloudInventory(page(), rejected('catalog unavailable'), inventory).textContent, /catalog unavailable/);
  assert.match(nodeInventory(page(), inventory).textContent, /spruce/);
  const cloud = cloudInventory(page(), fulfilled([{ key: 'remote-x', name: 'remote-x', kind: 'cloud', available: true }]), [rejected('nodes unavailable'), fulfilled(records)]);
  assert.match(cloud.textContent, /remote-xConfigured on coordinatorspruce: enabled \(last reported\)/);
});

test('cloud observations retain worker-only services absent from coordinator definitions', t => {
  installDOM(t);
  const observed = [{ ...nodes[1], providers: [{ key: 'worker-cloud', name: 'worker-cloud', kind: 'cloud', cloud: true }] }];
  const root = cloudInventory(page(), fulfilled([]), [fulfilled(observed), fulfilled([...records, record('worker-api', 'pine', true, false, 'cloud')])]);
  assert.match(root.textContent, /worker-cloudNot defined on coordinatorspruce: listed by node; enablement unavailable \(last reported\)/);
  assert.match(root.textContent, /worker-apiNot defined on coordinatorpine: enabled \(last reported\)/);
  const failedCatalog = cloudInventory(page(), rejected('catalog unavailable'), [fulfilled(observed), fulfilled(records)]);
  assert.match(failedCatalog.textContent, /catalog unavailable/);
  assert.match(failedCatalog.textContent, /worker-cloud/);
  assert.match(failedCatalog.textContent, /Coordinator configuration unavailable/);
  assert.doesNotMatch(failedCatalog.textContent, /Not defined on coordinator/);
});

for (const running of [false, true, undefined]) {
  test(`service running observation ${running} stays distinct from node health and absent process observations`, t => {
    installDOM(t);
    const provider = running === undefined ? local : { key: 'service-a', name: 'service-a', kind: 'external', cloud: false, running };
    for (const status of ['healthy', 'down', 'degraded']) {
      const observed = [{ name: 'atlas', cluster_role: 'coordinator', health_status: status, providers: [provider] }];
      const reports = [record(provider.key, 'atlas', true, true, provider.kind)];
      const summary = nodeInventory(page(), [fulfilled(observed), fulfilled(reports)]);
      const detail = nodeInventory(page(), [fulfilled(observed), fulfilled(reports)], provider);
      for (const root of [summary, detail]) {
        if (running === false) assert.match(root.textContent, /service not running/i);
        else if (running === true) {
          assert.match(root.textContent, /service running/i);
          assert.doesNotMatch(root.textContent, /not running/i);
        } else assert.doesNotMatch(root.textContent, /service running|not running/i);
        if (status === 'healthy') assert.doesNotMatch(root.textContent, /Last reported provider state/);
        else assert.match(root.textContent, /Last reported provider state/);
      }
      const chips = walk(summary).filter(node => node.tag === 'li');
      assert.equal(chips.length, 1);
    }
  });
}

test('cloud failures use unavailable copy when no node enablement can be observed', t => {
  installDOM(t);
  const configured = fulfilled([{ key: 'remote-x', name: 'remote-x', kind: 'cloud', is_cloud: true, available: true }]);
  const root = cloudInventory(page(), configured, [fulfilled(nodes), rejected('providers unavailable')]);
  assert.match(root.textContent, /remote-xConfigured on coordinatorNode enablement unavailable/);
  assert.doesNotMatch(root.textContent, /No node reports/);
  const empty = cloudInventory(page(), fulfilled([]), [rejected('nodes unavailable'), rejected('providers unavailable')]);
  assert.match(empty.textContent, /Cloud service status unavailable/);
  assert.doesNotMatch(empty.textContent, /No configured or enabled/);
});
