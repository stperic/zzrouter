import test from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { cytoscape, ELK } from '../vendor/graph.mjs';
import { computeCard, topologyData, visibleTopology, layoutInput, layoutPositions, topologySignature } from '../topology-data.mjs';
import { graphLayout, revealNode, initialCamera, activityMotion } from '../topology.mjs';
import { overview } from '../overview.mjs';
import { localProviderKey } from '../network-data.mjs';
import { find, button, installDOM, walk, drain } from './dom-fixture.mjs';

test('actual graph metrics preserve layout while idle and active evidence expire independently of polling', async t => {
  installDOM(t);
  const f = layoutFixture(t);
  let now = 100;
  const reduced = new EventTarget(); reduced.matches = true;
  const motion = activityMotion(f.cy, f.controller.signal, { now: () => now, hidden: () => false, reduced });
  const elements = [{ data: { id: 'worker', kind: 'compute', metrics: 'RAM 17 / 64 GiB', activityUntil: 200, active: false } }];
  const first = f.update(elements); f.pending[0].resolve(placed('worker', 20)); await first;
  f.cy.viewport({ zoom: 1.3, pan: { x: 90, y: 70 } });
  await f.update([{ data: { ...elements[0].data, metrics: 'RAM 23 / 64 GiB' } }]);
  assert.equal(f.pending.length, 1); assert.equal(f.cy.zoom(), 1.3);
  motion.pause(true); now = 201; motion.sync();
  assert.equal(f.cy.getElementById('metrics:worker').data('label'), 'RAM 23 / 64 GiB');
  await f.update([{ data: { ...elements[0].data, metrics: 'RAM 29 / 64 GiB', activityUntil: 300, active: true } }]);
  motion.pause(false); motion.sync();
  assert.equal(f.cy.getElementById('activity:worker').animated(), false, 'reduced motion disables animation');
  now = 301; motion.sync();
  assert.equal(f.cy.getElementById('metrics:worker').data('label'), 'RAM 29 / 64 GiB');
});

const ok = value => ({ status: 'fulfilled', value });
const node = { name: 'atlas', health_status: 'healthy', providers: [{ key: 'ollama', kind: 'external' }] };
const client = { name: '<img src=x>', key_id: 'one', source: 'managed', protocol: 'inference-api', authentication: 'api-key', active_requests: 0 };
const snapshot = { inventory: [ok([node, node, { name: 'spruce', health_status: 'down' }]), ok([{ name: 'ollama', node: 'atlas', kind: 'external', enabled: true }])],
  catalog: ok([{ key: 'groq', name: 'Groq', kind: 'cloud', available: true }]), clientResults: [ok({ data: [client] }), ok([])] };
const projected = () => topologyData(snapshot, 'atlas').elements;

test('collapsed provider branches preserve the complete model and stable selection identities', () => {
  const elements = projected(), before = structuredClone(elements);
  const collapsed = visibleTopology(elements, new Set());
  assert.equal(collapsed.some(({ data }) => data.kind === 'provider'), false);
  assert.equal(collapsed.some(({ data }) => data.target === localProviderKey('atlas', 'ollama')), false);
  const expanded = visibleTopology(elements, new Set(['atlas']));
  assert.ok(expanded.some(({ data }) => data.selection === localProviderKey('atlas', 'ollama')));
  assert.deepEqual(elements, before, 'presentation must not mutate inventory or erase hidden providers');
});

test('initial camera waits for a visible map and preserves readable zoom for large networks', t => {
  const cy = cytoscape({ headless: true, elements: [{ data: { id: 'coordinator' }, position: { x: 0, y: 0 } },
    { data: { id: 'node/worker' }, position: { x: 10000, y: 1000 } }], layout: { name: 'preset' } });
  t.after(() => cy.destroy());
  let width = 0;
  cy.width = () => width; cy.height = () => 600;
  assert.equal(initialCamera(cy), false);
  width = 1000;
  assert.equal(initialCamera(cy), true);
  assert.equal(cy.zoom(), 0.75);
  assert.equal(cy.minZoom(), cy.maxZoom());
  cy.zoom(1.5);
  assert.equal(cy.zoom(), 0.75);
  initialCamera(cy, 'node/worker');
  const point = cy.getElementById('node/worker').renderedPosition();
  assert.equal(point.x, 500);
  assert.equal(point.y, 300);
});

test('graph roles share physical selection and retain explicit health and client observations', () => {
  const { elements, nodes } = topologyData(snapshot, 'atlas');
  assert.equal(nodes.length, 2);
  const roles = elements.filter(({ data }) => data.selection === 'node/atlas');
  assert.equal(roles.length, 2);
  assert.notEqual(roles[0].data.id, roles[1].data.id);
  assert.match(roles[1].data.status, /Coordinator \+ Worker · Healthy/);
  assert.equal(elements.find(({ data }) => data.kind === 'client').data.status, 'No active requests');
  assert.equal(elements.find(({ data }) => data.kind === 'client').data.name, 'Key: <img src=x>');
  const local = elements.find(({ data }) => data.kind === 'provider');
  assert.equal(local.data.providerKey, 'ollama');
  assert.equal(elements.find(({ data }) => data.kind === 'cloud').data.providerKey, 'groq');
  assert.ok(elements.some(({ data }) => data.source === 'node/atlas' && data.target === local.data.id));
  const unknown = topologyData({ ...snapshot, inventory: [{ status: 'rejected' }, ok([])] }, 'atlas');
  assert.equal(unknown.elements.find(({ data }) => data.id === 'coordinator').data.status, 'Coordinator · Unknown');
});

test('the tree hides disabled providers while preserving unknown enablement and full management identities', () => {
  const inventory = [ok([node, { name: 'studio/one %', health_status: 'down', providers: [
    { key: 'ollama', kind: 'external' }, { key: 'vllm/custom%', kind: 'python' }, { key: 'huggingface', kind: 'registry' },
  ] }]), ok([...snapshot.inventory[1].value, { node: 'studio/one %', name: 'vllm/custom%', kind: 'python', enabled: false }])];
  const { elements, providers: inventoryProviders } = topologyData({ ...snapshot, inventory }, 'atlas');
  const providers = elements.filter(({ data }) => data.kind === 'provider');
  assert.equal(providers.length, 2);
  assert.equal(new Set(providers.map(({ data }) => data.selection)).size, 2);
  assert.equal(inventoryProviders.length, 3);
  for (const { data } of providers) {
    assert.equal(data.selection, localProviderKey(data.nodeName, data.providerKey));
    assert.ok(elements.some(({ data: edge }) => edge.source === `node/${data.nodeName}` && edge.target === data.id));
  }
  assert.ok(!providers.some(({ data }) => data.providerKey === 'vllm/custom%'));
  assert.ok(!elements.some(({ data }) => data.target === localProviderKey('studio/one %', 'vllm/custom%')));
  assert.equal(providers.find(({ data }) => data.nodeName === 'studio/one %' && data.providerKey === 'ollama').data.status, 'Unknown');
  assert.notEqual(localProviderKey('a/b', 'c'), localProviderKey('a', 'b/c'));
  assert.equal(topologyData({ ...snapshot, inventory: [inventory[0], { status: 'rejected' }] }, 'atlas').elements.filter(({ data }) => data.kind === 'provider').length, 3);
});

test('vendored ELK lays out the actual compound projection with finite distinct positions', async () => {
  const { Worker } = createRequire(import.meta.url)('../vendor/elk-worker.min.js');
  const elk = new ELK({ algorithms: ['layered'], workerFactory: () => new Worker() });
  const elements = projected();
  const result = await elk.layout(layoutInput(elements));
  const positions = layoutPositions(result);
  const leaves = elements.filter(({ data }) => !data.source && data.kind !== 'group');
  assert.equal(positions.size, leaves.length);
  assert.equal(new Set([...positions.values()].map(point => `${point.x}:${point.y}`)).size, leaves.length);
  for (const point of positions.values()) assert.ok(Number.isFinite(point.x) && Number.isFinite(point.y));
  assert.ok(positions.get('node/atlas').y > positions.get('coordinator').y);
  assert.ok(positions.get(localProviderKey('atlas', 'ollama')).y > positions.get('node/atlas').y);
});

test('layout reserves the full compute card above its provider icons', async () => {
  const { Worker } = createRequire(import.meta.url)('../vendor/elk-worker.min.js');
  const elk = new ELK({ algorithms: ['layered'], workerFactory: () => new Worker() });
  const input = layoutInput(projected(), new Map([['node/atlas', { w: 230, h: 210 }]]));
  const compute = input.children.find(node => node.id === 'compute');
  const parent = compute.children.find(node => node.id === 'node/atlas');
  assert.equal(parent.width, computeCard.width);
  assert.equal(parent.height, 226);
  const result = await elk.layout(input);
  const positions = layoutPositions(result);
  const providerTop = positions.get(localProviderKey('atlas', 'ollama')).y - 21;
  const cardBottom = positions.get('node/atlas').y + computeCard.height / 2;
  assert.ok(providerTop > cardBottom, 'the full machine card must not cover its provider icon');
  const placed = result.children.find(node => node.id === 'compute');
  const cell = placed.children.find(node => node.id === 'node/atlas');
  assert.equal(positions.get('node/atlas').y, placed.y + cell.y + cell.height / 2);
});

function layoutFixture(t) {
  const cy = cytoscape({ headless: true, elements: [], layout: { name: 'preset' } });
  const controller = new AbortController();
  t.after(() => { controller.abort(); cy.destroy(); });
  const pending = [];
  const errors = [];
  const elk = { layout: () => new Promise((resolve, reject) => pending.push({ resolve, reject })) };
  const update = graphLayout(cy, elk, controller.signal, error => errors.push(error));
  return { cy, controller, update, pending, errors };
}
const placed = (id, x) => ({ children: [{ id, x, y: 20, width: 100, height: 100 }] });

test('status refresh preserves viewport and positions, membership reruns layout', async t => {
  const f = layoutFixture(t);
  const elements = [{ data: { id: 'atlas', kind: 'compute', status: 'Healthy' } }];
  const first = f.update(elements);
  f.pending[0].resolve(placed('atlas', 20));
  await first;
  f.cy.viewport({ zoom: 1.3, pan: { x: 90, y: 70 } });
  const position = f.cy.getElementById('atlas').position();
  await f.update([{ data: { ...elements[0].data, status: 'Down' } }]);
  assert.equal(f.pending.length, 1);
  assert.equal(f.cy.getElementById('atlas').data('status'), 'Down');
  assert.deepEqual(f.cy.getElementById('atlas').position(), position);
  assert.equal(f.cy.zoom(), 1.3);
  assert.deepEqual(f.cy.pan(), { x: 90, y: 70 });
});

test('label growth reruns layout, while label changes within the reserved cell keep positions', async t => {
  const f = layoutFixture(t);
  const elements = [{ data: { id: 'atlas', kind: 'compute', label: 'Short' } }];
  const first = f.update(elements);
  f.pending[0].resolve(placed('atlas', 20));
  await first;
  const node = f.cy.getElementById('atlas');
  node.boundingBox = () => ({ w: 180, h: 100 });
  await f.update([{ data: { ...elements[0].data, label: 'Another short name' } }]);
  assert.equal(f.pending.length, 1);
  f.cy.viewport({ zoom: 1.3, pan: { x: 90, y: 70 } });
  node.boundingBox = () => ({ w: 225, h: 210 });
  const grown = f.update([{ data: { ...elements[0].data, label: 'A longer wrapped display name' } }]);
  assert.equal(f.pending.length, 2);
  f.pending[1].resolve(placed('atlas', 60));
  await grown;
  assert.equal(f.cy.zoom(), 1.3);
  assert.deepEqual(f.cy.pan(), { x: 90, y: 70 });
});

test('identical updates wait for the committed layout instead of reporting premature readiness', async t => {
  const f = layoutFixture(t);
  const elements = [{ data: { id: 'atlas', kind: 'compute' } }];
  const first = f.update(elements);
  let finished = false;
  const second = f.update(elements).then(value => { finished = true; return value; });
  await drain();
  assert.equal(finished, false);
  assert.equal(f.pending.length, 1);
  f.pending[0].resolve(placed('atlas', 60));
  assert.deepEqual(await Promise.all([first, second]), [true, true]);
});

test('a narrowed viewport reveals the selected icon without changing zoom or moving visible items', t => {
  const cy = cytoscape({ headless: true, elements: [{ data: { id: 'left' }, position: { x: -100, y: 100 } }], layout: { name: 'preset' } });
  t.after(() => cy.destroy());
  cy.width = () => 600;
  cy.height = () => 400;
  cy.zoom(1.2);
  revealNode(cy, 'left');
  assert.equal(cy.zoom(), 1.2);
  assert.ok(cy.getElementById('left').renderedBoundingBox().x1 >= 40);
  const pan = { ...cy.pan() };
  revealNode(cy, 'left');
  assert.deepEqual(cy.pan(), pan);
  cy.getElementById('left').position({ x: 900, y: 600 });
  revealNode(cy, 'left');
  const box = cy.getElementById('left').renderedBoundingBox();
  assert.ok(box.x2 <= 560 && box.y2 <= 360);
});

test('newer layout wins even if old layout finishes last, and abort rejects completion', async t => {
  const f = layoutFixture(t);
  const first = f.update([{ data: { id: 'old' } }]);
  const second = f.update([{ data: { id: 'new' } }]);
  f.pending[1].resolve(placed('new', 180));
  await second;
  const position = { ...f.cy.getElementById('new').position() };
  f.pending[0].resolve(placed('old', 10));
  await first;
  assert.deepEqual(f.cy.getElementById('new').position(), position);
  const third = f.update([{ data: { id: 'last' } }]);
  f.controller.abort();
  f.pending[2].resolve(placed('last', 900));
  await third;
  assert.equal(f.cy.getElementById('last').position().x, 0);
});

test('failed layout is reported and can be attempted on explicit refresh', async t => {
  const f = layoutFixture(t);
  const elements = [{ data: { id: 'atlas' } }];
  const first = f.update(elements);
  f.pending[0].reject(new Error('Layout failed'));
  await first;
  assert.equal(f.errors.length, 1);
  const second = f.update(elements);
  f.pending[1].resolve(placed('atlas', 20));
  await second;
  assert.equal(f.cy.getElementById('atlas').position().x, 70);
  assert.notEqual(topologySignature([{ data: { id: 'edge', source: 'a', target: 'b' } }]), topologySignature([{ data: { id: 'edge', source: 'a', target: 'c' } }]));
});

test('overview switches one nonmodal drawer without replacing the map and protects pending saves', async t => {
  const root = installDOM(t);
  const controller = new AbortController();
  t.after(() => controller.abort());
  let select;
  let canLeave = true;
  const p = { root, identity: { role: 'admin' }, state: { node_name: 'atlas' }, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, syncActivity() {},
    go: (_, value) => select(value), handleSelection: callback => { select = callback; },
    api: { canLeave: () => canLeave, canChange: () => true,
      list: async path => path === '/nodes' ? snapshot.inventory[0].value : path === '/providers' ? snapshot.inventory[1].value : path.startsWith('/providers/catalog') ? snapshot.catalog.value : [],
      request: async () => snapshot.clientResults[0].value } };
  await overview(p);
  const map = find(root, element => element.getAttribute('aria-label') === 'Network map');
  select('node/atlas');
  const firstPanel = find(root, element => element.tag === 'dialog');
  assert.equal(firstPanel.getAttribute('open'), '');
  assert.equal(firstPanel.getAttribute('aria-modal'), undefined);
  find(root, element => element.tag === 'button' && ['Edit', 'Edit node settings'].includes(element.textContent)).dispatchEvent(new Event('click'));
  select('node/spruce');
  assert.equal(root.contains(firstPanel), false);
  assert.ok(root.contains(map));
  assert.equal(walk(root).filter(element => element.tag === 'dialog').length, 1);
  assert.match(find(root, element => element.tag === 'dialog').textContent, /spruce/);
  canLeave = false;
  select('node/atlas');
  assert.match(find(root, element => element.tag === 'dialog').textContent, /spruce/);
  const escape = new Event('keydown', { cancelable: true });
  Object.defineProperty(escape, 'key', { value: 'Escape' });
  window.dispatchEvent(escape);
  assert.ok(walk(root).some(element => element.tag === 'dialog'));
  canLeave = true;
  window.dispatchEvent(escape);
  assert.ok(!walk(root).some(element => element.tag === 'dialog'));
  await drain();
  assert.match(root.textContent, /network map is unavailable/, 'a renderer failure retains the accessible entity list');
  const listNode = find(root, element => element.getAttribute('data-selection') === 'node/atlas');
  listNode.dispatchEvent(new Event('click'));
  assert.match(find(root, element => element.tag === 'dialog').textContent, /atlas/);
});

test('Refresh updates details and a successful save waits for authoritative node readback', async t => {
  const root = installDOM(t);
  const controller = new AbortController();
  t.after(() => controller.abort());
  let select, heldRead;
  let observed = { ...node };
  const provider = { name: 'ollama', node: 'atlas', kind: 'external', enabled: true, installed: true };
  const p = { root, identity: { role: 'admin' }, state: { node_name: 'atlas' }, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, syncActivity() {},
    go: (_, value) => select(value), handleSelection: callback => { select = callback; },
    api: { canLeave: () => true, canChange: () => true,
      list: async path => path === '/nodes' ? heldRead ? await heldRead : [observed] : path.startsWith('/providers/catalog') ? [] : path.startsWith('/providers') ? [{ ...provider }] : [],
      request: async () => ({ data: [] }), data: async (_path, options = {}) => { if (options.method) provider.enabled = false; return {}; } } };
  await overview(p);
  select('node/atlas');
  observed = { ...observed, health_status: 'down' };
  button(root, 'Refresh').dispatchEvent(new Event('click'));
  await drain();
  assert.match(find(root, element => element.tag === 'dialog').textContent, /Coordinator \+ WorkerDown/);
  find(root, element => element.tag === 'button' && ['Edit', 'Edit node settings'].includes(element.textContent)).dispatchEvent(new Event('click'));
  const form = find(root, element => element.tag === 'form');
  const checkbox = find(root, element => element.type === 'checkbox');
  checkbox.checked = false;
  checkbox.dispatchEvent(new Event('change'));
  let release;
  heldRead = new Promise(resolve => { release = resolve; });
  form.dispatchEvent(new Event('submit', { cancelable: true }));
  await drain();
  assert.ok(!root.contains(form), 'a saved form is replaced by readback status');
  assert.match(root.textContent, /Provider settings saved/);
  observed = { ...observed, health_status: 'healthy' };
  release([observed]);
  await drain();
  assert.ok(!root.contains(form));
  assert.match(find(root, element => element.tag === 'dialog').textContent, /Coordinator \+ WorkerHealthy/);
  assert.match(find(root, element => element.tag === 'dialog').textContent, /ollamaDisabled/);
});

test('local provider deep links and list selection keep the machine context in one docked panel', async t => {
  const root = installDOM(t);
  const controller = new AbortController();
  t.after(() => controller.abort());
  const machine = 'studio/one %';
  let observations = [node, { name: machine, health_status: 'down', providers: [{ key: 'ollama', name: 'Ollama', kind: 'external', running: true }] }];
  let records = [...snapshot.inventory[1].value, { node: machine, name: 'ollama', kind: 'external', enabled: false, installed: true }];
  let select;
  let route = localProviderKey(machine, 'ollama');
  const writes = [];
  const p = { root, identity: { role: 'admin' }, state: { node_name: 'atlas' }, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, syncActivity() {},
    go: (_, value) => { route = value; select(value); }, handleSelection: callback => { select = callback; },
    api: { canLeave: () => true, canChange: () => true,
      list: async path => path === '/nodes' ? observations : path.startsWith('/providers/catalog') ? [] : path.startsWith('/providers') ? records : [], request: async () => ({ data: [] }),
      data: async (path, options = {}) => { if (!options.method) return {}; writes.push(path); records.find(row => row.node === machine).enabled = options.body.enabled; } } };
  const selection = localProviderKey(machine, 'ollama');
  await overview(p, selection);
  const map = find(root, element => element.getAttribute('aria-label') === 'Network map');
  let panel = find(root, element => element.tag === 'dialog');
  assert.match(panel.textContent, /Local provider on studio\/one %/);
  assert.match(panel.textContent, /EnablementDisabledInstallationInstalledServiceService running/);
  assert.match(panel.textContent, /last reported observations/);
  assert.ok(!walk(root).some(element => element.getAttribute('data-selection') === selection));
  select(localProviderKey('atlas', 'ollama'));
  panel = find(root, element => element.tag === 'dialog');
  assert.match(panel.textContent, /Local provider on atlas/);
  assert.doesNotMatch(panel.textContent, /studio|last reported observations/);
  assert.ok(root.contains(map));
  select(selection);
  find(root, element => element.tag === 'button' && ['Edit', 'Edit node settings'].includes(element.textContent)).dispatchEvent(new Event('click'));
  assert.match(find(root, element => element.tag === 'form').textContent, /Settings for studio\/one %/);
  button(root, 'Cancel').dispatchEvent(new Event('click'));
  assert.match(find(root, element => element.tag === 'dialog').textContent, /Local provider on studio\/one %/);
  button(root, 'View node').dispatchEvent(new Event('click'));
  button(root, 'Provider details').dispatchEvent(new Event('click'));
  assert.match(find(root, element => element.tag === 'dialog').textContent, /Local provider on studio\/one %/);
  find(root, element => element.tag === 'button' && ['Edit', 'Edit node settings'].includes(element.textContent)).dispatchEvent(new Event('click'));
  const checkbox = find(root, element => element.type === 'checkbox');
  checkbox.checked = true;
  checkbox.dispatchEvent(new Event('change'));
  find(root, element => element.tag === 'form').dispatchEvent(new Event('submit', { cancelable: true }));
  await drain();
  assert.deepEqual(writes, [`/providers/ollama?node=${encodeURIComponent(machine)}`]);
  await drain();
  assert.equal(route, selection);
  assert.match(find(root, element => element.tag === 'dialog').textContent, /Local provider on studio\/one %/);
  assert.match(find(root, element => element.tag === 'dialog').textContent, /EnablementEnabled/);
  assert.equal(find(root, element => element.getAttribute('data-selection') === selection).getAttribute('aria-pressed'), 'true');
  records = records.filter(row => row.node !== machine);
  observations = [node, { name: machine, health_status: 'healthy', providers: [] }];
  button(root, 'Refresh').dispatchEvent(new Event('click'));
  await drain();
  assert.match(find(root, element => element.tag === 'dialog').textContent, /provider is no longer in the current node inventory/);
  assert.equal(walk(root).filter(element => element.tag === 'dialog').length, 1);
});
