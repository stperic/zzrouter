import test from 'node:test';
import assert from 'node:assert/strict';
import { pricing, priceOverrideForm, priceOverrideValues, priceOverrideChanged, priceOverrideKey } from '../pricing.mjs';
import { APIError } from '../api.mjs';
import { installDOM, named, find, button, drain, walk } from './dom-fixture.mjs';

const record = { provider: 'engine', model: 'org/model', input_cost_per_1m: 1.25, output_cost_per_1m: 2.5, cache_read_cost_per_1m: 0.125, note: 'Contract', updated_at: '2026-11-01T00:00:00Z', updated_by: 'key:admin', billed_per_token: true };
function fixture(t, handle) {
  const root = installDOM(t), controller = new AbortController(), calls = [], navigation = [];
  let handler, accepts;
  const p = { root, scope: { signal: controller.signal }, current: () => !controller.signal.aborted, syncActivity() {}, close() {},
    go: (...args) => navigation.push(args), handleSelection: (callback, predicate) => { handler = callback; accepts = predicate; }, selectSection() {},
    api: { canLeave: () => true, canChange: () => true,
      data: async (path, options = {}) => {
        calls.push({ path, ...options });
        if (handle) { const result = await handle(path, options); if (result !== undefined) return result; }
        if (options.method) return structuredClone(record);
        if (path === '/pricing/status') return { model_count: 1500, fetched_at: '2026-11-01T00:00:00Z', unpriced_count: 1, unpriced: { 'unknown/model': 2 } };
        if (path === '/pricing/overrides') return { overrides: [structuredClone(record)], count: 1 };
        if (path.startsWith('/pricing?')) return structuredClone(record);
        assert.fail(path);
      },
      request: async (path, options = {}) => { calls.push({ path, ...options }); return { data: [structuredClone(record)], total: 75, has_more: true }; },
    } };
  t.after(() => controller.abort());
  return { root, p, controller, calls, navigation, select: (...args) => handler(...args), accepts: value => accepts(value) };
}
const editor = root => find(root, item => item.tag === 'form' && item.className === 'entity-editor pricing-editor');
const submit = root => editor(root).listeners.get('submit')({ preventDefault() {} });
const click = async (root, label) => { button(root, label).dispatchEvent(new Event('click')); await drain(); };

test('unavailable pricing replaces the loading state and offers a read retry', async t => {
  const f = fixture(t, async path => {
    if (path === '/pricing/status') throw new APIError('Pricing is disabled.', { status: 503 });
  });
  let reloaded = false;
  f.p.reload = () => { reloaded = true; };
  await pricing(f.p);
  assert.doesNotMatch(f.root.textContent, /Loading pricing/);
  assert.match(f.root.textContent, /Pricing is disabled/);
  await click(f.root, 'Retry pricing');
  assert.equal(reloaded, true);
});

test('price editable projection preserves cache-read and excludes response-only fields', () => {
  assert.deepEqual(priceOverrideValues(record), { provider: 'engine', model: 'org/model', input_cost_per_1m: 1.25, output_cost_per_1m: 2.5, cache_read_cost_per_1m: 0.125, note: 'Contract' });
  assert.equal(priceOverrideChanged(record, { ...record, updated_at: 'later' }), false);
  assert.equal(priceOverrideChanged(record, { ...record, cache_read_cost_per_1m: 0 }), true);
  assert.equal(priceOverrideChanged(record, undefined), true);
  assert.equal(priceOverrideKey({ provider: ' Engine ', model: ' Org/Model ' }), 'engine/org/model');
});

test('note-only price edit retains cached input and uses per-million units unchanged', async t => {
  const f = fixture(t); let saved;
  f.root.append(priceOverrideForm(f.p, record, { saved: body => { saved = body; }, reload() {} }));
  named(f.root, 'price_note').value = 'Updated contract'; await submit(f.root);
  const write = f.calls.find(call => call.method === 'POST');
  assert.deepEqual(write.body, { ...priceOverrideValues(record), note: 'Updated contract' });
  assert.equal(write.body.cache_read_cost_per_1m, 0.125);
  assert.equal(saved.note, 'Updated contract');
  assert.equal(f.calls[0].path, '/pricing/overrides', 'fresh evidence precedes upsert');
});

test('new price overrides require explicit rates and preserve intentional zeros', async t => {
  const f = fixture(t, async path => path === '/pricing/overrides' ? { overrides: [] } : undefined);
  f.root.append(priceOverrideForm(f.p, null, { saved() {}, reload() {} }));
  assert.equal(named(f.root, 'price_input_cost_per_1m').value, '');
  named(f.root, 'price_provider').value = 'local-engine'; named(f.root, 'price_model').value = '*';
  for (const name of ['input', 'output', 'cache_read']) named(f.root, `price_${name}_cost_per_1m`).value = '0';
  await submit(f.root);
  assert.deepEqual(f.calls.find(call => call.method === 'POST').body, { provider: 'local-engine', model: '*', input_cost_per_1m: 0, output_cost_per_1m: 0, cache_read_cost_per_1m: 0, note: '' });
});

test('concurrent override edits stop saves and cannot overwrite new cache rates', async t => {
  const f = fixture(t, async path => path === '/pricing/overrides' ? { overrides: [{ ...record, cache_read_cost_per_1m: 0.25 }] } : undefined);
  f.root.append(priceOverrideForm(f.p, record, { saved() {}, reload() {} }));
  named(f.root, 'price_note').value = 'Changed'; await submit(f.root); await submit(f.root);
  assert.equal(f.calls.filter(call => call.method).length, 0);
  assert.equal(button(f.root, 'Save changes').disabled, true);
  assert.match(f.root.textContent, /changed in another session/);
});

test('create detects an existing case-insensitive identity instead of silently upserting', async t => {
  const f = fixture(t);
  f.root.append(priceOverrideForm(f.p, null, { saved() {}, reload() {} }));
  named(f.root, 'price_provider').value = ' ENGINE '; named(f.root, 'price_model').value = 'ORG/MODEL';
  await submit(f.root);
  assert.equal(f.calls.filter(call => call.method).length, 0);
  assert.match(f.root.textContent, /already exists/);
});

test('price deletion uses the exact slash-containing identity after rereading', async t => {
  const f = fixture(t); let deleted = false;
  f.root.append(priceOverrideForm(f.p, record, { saved: (_body, flag) => { deleted = flag; }, reload() {} }));
  await click(f.root, 'Delete override');
  const write = f.calls.find(call => call.method === 'DELETE');
  assert.equal(write.path, '/pricing/overrides?provider=engine&model=org%2Fmodel');
  assert.equal(write.body, undefined); assert.equal(deleted, true);
});

test('uncertain pricing writes lock the form and offer inspection without replay', async t => {
  const f = fixture(t, async (_path, options) => { if (options.method) throw new APIError('Connection lost', { uncertain: true }); });
  f.root.append(priceOverrideForm(f.p, record, { saved() {}, reload() {} }));
  named(f.root, 'price_note').value = 'Updated'; await submit(f.root); await submit(f.root);
  assert.equal(f.calls.filter(call => call.method).length, 1);
  assert.equal(button(f.root, 'Save changes').disabled, true);
  assert.ok(button(f.root, 'Inspect current pricing'));
});

test('successful price write followed by failed readback leaves only recovery controls', async t => {
  const f = fixture(t);
  f.root.append(priceOverrideForm(f.p, record, { saved() { throw new Error('Read unavailable'); }, reload() {} }));
  named(f.root, 'price_note').value = 'Updated'; await submit(f.root);
  assert.ok(button(f.root, 'Reload details'));
  assert.equal(walk(f.root).some(item => item.tag === 'form'), false);
  assert.match(f.root.textContent, /Price override saved/);
});

test('System pricing uses the shared details/editor pattern and permits leaving its subsection', async t => {
  const f = fixture(t); await pricing(f.p, 'engine/org%2Fmodel');
  assert.equal(f.accepts('pricing'), true); assert.equal(f.accepts('pricing/engine/org%2Fmodel'), true);
  assert.equal(f.accepts('updates'), false); assert.equal(f.accepts('general'), false);
  assert.match(f.root.textContent, /Price override/); await click(f.root, 'Edit');
  assert.equal(named(f.root, 'price_model').readOnly, true);
  assert.equal(named(f.root, 'price_cache_read_cost_per_1m').value, '0.125');
});

test('catalog search respects the API result cap and keeps exact query identity', async t => {
  const f = fixture(t); await pricing(f.p); await click(f.root, 'Catalog');
  named(f.root, 'price_query').value = 'org/model';
  const form = find(f.root, item => item.tag === 'form' && item.className === 'pricing-catalog');
  await form.listeners.get('submit')({ preventDefault() {} });
  const searches = f.calls.filter(call => call.path.startsWith('/zzrouter/v1/pricing?'));
  assert.equal(searches.length, 1); assert.match(searches[0].path, /q=org%2Fmodel/);
  assert.match(f.root.textContent, /1 of 75 results/); assert.match(f.root.textContent, /up to 50/);
  await click(f.root, 'org/model');
  assert.deepEqual(f.navigation.at(-1), ['settings', 'pricing/engine/org%2Fmodel', 'catalog']);
});

test('catalog inspection reads effective prices and links an existing override', async t => {
  const f = fixture(t); await pricing(f.p, 'engine/org%2Fmodel', 'catalog');
  assert.ok(f.calls.some(call => call.path === '/pricing?provider=engine&model=org%2Fmodel'));
  assert.ok(button(f.root, 'View override')); assert.match(f.root.textContent, /effective rates returned by the pricing API/);
});

test('unpriced observations retain ambiguous names without inventing provider assignments', async t => {
  const f = fixture(t); await pricing(f.p); await click(f.root, 'Unpriced (1)');
  assert.match(f.root.textContent, /unknown\/model/); assert.match(f.root.textContent, /refused budget admissions/);
  await click(f.root, 'Create a price override'); assert.deepEqual(f.navigation.at(-1), ['settings', 'pricing', 'create']);
});
