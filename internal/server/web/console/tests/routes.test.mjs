import test from 'node:test';
import assert from 'node:assert/strict';
import { routeForm } from '../route-form.mjs';
import { routes } from '../routes.mjs';
import { routeFields } from '../access-policy.mjs';
import { installDOM, named, button, find, drain } from './dom-fixture.mjs';

const catalog = [{ key: 'ollama', name: 'Ollama', kind: 'external', protocol: 'ollama' }, { key: 'groq', name: 'Groq', kind: 'cloud', protocol: 'openai' }];
catalog.push({ key: 'huggingface', kind: 'registry', protocol: '' }, { key: 'comfyui', kind: 'python', protocol: '' });
const team = { id: 'engineering', name: 'Engineering', kind: 'shared', route_access: { mode: 'selected', routes: ['coding'] } };
const route = { name: 'coding', description: 'Shared coding route', strategy: 'priority', replicas: [{ name: 'primary', provider: 'groq', model: 'old-model', node: 'worker', priority: 1, on_demand: false, tags: ['fast'], timeout: '30s', params: { temperature: 0.4 } }] };
function page(t, data) {
 const root = installDOM(t), abort = new AbortController(), calls = [];
 const p = { root, scope: { signal: abort.signal }, current: () => !abort.signal.aborted, identity: {}, state: { node_name: 'coord' }, go() {}, syncActivity() {}, api: {
  canChange: () => true, canLeave: () => true,
  list: async path => ({ '/model-groups': [structuredClone(route)], '/teams': [team], '/providers/catalog?kind=all': catalog, '/models': [], '/nodes': [{ name: 'worker' }] })[path],
  data: async (path, options = {}) => {
   calls.push({ path, ...options });
   if (data) { const value = await data(path, options); if (value !== undefined) return value; }
   if (options.method) return {};
   if (path.endsWith('/state')) return { replicas: [{ health_state: 'healthy' }] };
   options.onVersion?.('"version-1"'); return structuredClone(route);
  },
 } };
 t.after(() => abort.abort()); return { root, p, calls, abort };
}
const submit = async form => { form.dispatchEvent(new Event('submit', { cancelable: true })); await drain(); };
const editor = root => find(root, node => node.tag === 'form' && node.className?.includes('route-editor'));
const previewForm = root => find(root, node => node.tag === 'form' && node.className?.includes('route-preview'));
const options = { catalog, models: [], nodes: [{ name: 'worker' }], etag: '"version-1"', teams: [] };

test('description-only edit preserves worker-pinned cloud destinations and advanced settings', async t => {
 const f = page(t); f.root.append(routeForm(f.p, structuredClone(route), options, () => {}));
 assert.equal(named(f.root, 'node_0').value, 'worker');
 assert.doesNotMatch(named(f.root, 'provider_0').textContent, /huggingface|comfyui/);
 named(f.root, 'route_description').value = 'Renamed'; await submit(editor(f.root));
 const call = f.calls[0]; assert.equal(call.method, 'PATCH'); assert.equal(call.ifMatch, '"version-1"'); assert.deepEqual(call.body, { description: 'Renamed' });
});

test('editing a model retains provider, node and advanced destination settings', async t => {
 const f = page(t); f.root.append(routeForm(f.p, structuredClone(route), options, () => {}));
 named(f.root, 'model_0').value = 'new-model'; await submit(editor(f.root));
 assert.deepEqual(f.calls[0].body.replicas, [{ ...route.replicas[0], model: 'new-model' }]);
});

test('route creation and stale edits use explicit version conditions with no automatic retry', async t => {
 const f = page(t); f.root.append(routeForm(f.p, null, options, () => {}));
 named(f.root, 'route_name').value = 'new-route'; named(f.root, 'provider_0').value = 'ollama'; named(f.root, 'model_0').value = 'local';
 await submit(editor(f.root)); assert.equal(f.calls[0].method, 'PUT'); assert.equal(f.calls[0].ifNoneMatch, '*'); assert.equal(f.calls[0].path, '/model-groups/new-route');
 const conflict = page(t, async () => { throw Object.assign(new Error('Changed by another administrator'), { status: 412 }); });
 conflict.root.append(routeForm(conflict.p, structuredClone(route), options, () => {}));
 named(conflict.root, 'route_description').value = 'Changed'; await submit(editor(conflict.root)); await submit(editor(conflict.root));
 assert.equal(conflict.calls.length, 1); assert.ok(button(conflict.root, 'Reload routes'));
});

test('assigned routes cannot be deleted and saved choices survive unavailable inventory', t => {
 const f = page(t); f.root.append(routeForm(f.p, structuredClone(route), { ...options, teams: [team], catalog: [], nodes: [], incomplete: true }, () => {}));
 assert.equal(button(f.root, 'Delete route').disabled, true); assert.equal(named(f.root, 'provider_0').value, 'groq'); assert.equal(named(f.root, 'node_0').value, 'worker');
 assert.match(f.root.textContent, /Saved destinations are retained/);
});

test('route picker preserves unavailable assignments and filtered selections without widening access', t => {
 const root = installDOM(t); const fields = routeFields({ route_access: { mode: 'selected', routes: ['missing'] } }, [route]); root.append(fields.root);
 named(root, 'assigned_route_search').value = 'coding'; named(root, 'assigned_route_search').dispatchEvent(new Event('input'));
 const checkbox = named(root, 'assigned_route'); checkbox.checked = true; checkbox.dispatchEvent(new Event('change'));
 assert.deepEqual(fields.read(), { mode: 'selected', routes: ['coding', 'missing'] });
 const empty = routeFields({}, []); named(empty.root, 'route_mode').value = 'selected'; assert.throws(() => empty.read(), /at least one route/);
 named(empty.root, 'route_mode').value = 'none'; assert.deepEqual(empty.read(), { mode: 'none' });
});

test('assignment refuses to replace a team route policy changed by another administrator', async t => {
 const f = page(t, async path => {
  if (path === '/teams/engineering') return { ...team, route_access: { mode: 'selected', routes: ['other'] } };
 });
 await routes(f.p, 'coding');
 button(f.root, 'Manage team assignments').dispatchEvent(new Event('click'));
 const choice = named(f.root, 'assignment_team');
 choice.value = 'engineering';
 await submit(find(f.root, node => node.tag === 'form' && node.contains(choice)));
 named(f.root, 'route_mode').value = 'none';
 await submit(find(f.root, node => node.tag === 'form' && node.contains(named(f.root, 'route_mode'))));
 assert.match(f.root.textContent, /An edited field changed on the server/);
 assert.equal(button(f.root, 'Save route access').disabled, true);
 assert.equal(f.calls.some(call => call.method === 'PATCH'), false);
});

test('a preview from another route version cannot display stale destination labels', async t => {
 const f = page(t, async path => path.endsWith('/preview') ? { assignments: [{ route: 'coding', route_version: '"version-2"', replica: 'primary' }] } : undefined);
 await routes(f.p, 'coding'); await submit(previewForm(f.root));
 assert.match(f.root.textContent, /Reload the route to preview its current destinations/); assert.doesNotMatch(f.root.textContent, /Preferred destination/);
});

test('saving a route cancels pending preview and clears completed preview results', async t => {
 let resolve, request;
 const pending = new Promise(done => { resolve = done; });
 const f = page(t, async (path, opts) => { if (path.endsWith('/preview')) { request = opts; return pending; } });
 await routes(f.p, 'coding'); await submit(previewForm(f.root));
 button(f.root, 'Edit').dispatchEvent(new Event('click'));
 named(f.root, 'model_0').value = 'new-model'; await submit(editor(f.root)); assert.equal(request.signal.aborted, true);
 resolve({ assignments: [{ route: 'coding', route_version: '"version-1"', replica: 'primary' }] }); await drain();
 assert.match(f.root.textContent, /Route saved/); assert.doesNotMatch(f.root.textContent, /Preferred destination/);
 assert.equal(f.calls.filter(call => call.path.endsWith('/preview')).length, 1);
});

test('team preview remains a read-only API operation and uses the evaluated route version', async t => {
 const f = page(t, async path => path.endsWith('/preview') ? { assignments: [{ route: 'coding', route_version: '"version-1"', replica: 'primary', projected_cost_micro: 25, projected_cost_source: 'pricing' }], unchecked: ['key_limits'] } : undefined);
 await routes(f.p, 'coding'); await submit(previewForm(f.root));
 const call = f.calls.find(item => item.path.endsWith('/preview')); assert.equal(call.effectful, false); assert.equal(call.body.team_id, 'engineering');
 assert.match(f.root.textContent, /Preferred destination: groq · old-model on worker/); assert.match(f.root.textContent, /Not checked: key limits/);
 button(f.root, 'Edit').dispatchEvent(new Event('click'));
 named(f.root, 'model_0').value = 'new-model'; await submit(editor(f.root)); assert.doesNotMatch(f.root.textContent, /Preferred destination/);
});

test('node selection includes worker-only cloud services and assigned-model providers', t => {
 const f = page(t);
 f.root.append(routeForm(f.p, null, { ...options,
  nodes: [{name:'coord', health_status:'healthy', providers:[{key:'groq',kind:'cloud',state:'cloud-available'}]},
   {name:'worker', health_status:'healthy', providers:[{key:'private-cloud',kind:'cloud',cloud:true},
     {key:'private-engine',kind:'external'}, {key:'browser-tool',kind:'python'}, {key:'ollama',state:'live',running:false}]},
   {name:'offline', health_status:'down', providers:[{key:'ollama',state:'live'}]}],
  models: [{name:'local',node:'worker',assigned_app:'private-engine'}],
 }, () => {}));
 assert.match(named(f.root, 'provider_0').textContent, /Groq/);
 assert.doesNotMatch(named(f.root, 'provider_0').textContent, /Ollama|private-cloud|private-engine|browser-tool/);
 const nodes = named(f.root, 'node_0').children;
 assert.equal(nodes.find(option => option.value === 'offline').disabled, true);
 assert.equal(nodes.some(option => option.value === 'coord'), false);
 named(f.root, 'node_0').value = 'worker'; named(f.root, 'node_0').dispatchEvent(new Event('change'));
 assert.match(named(f.root, 'provider_0').textContent, /private-engine \(Local\).*Groq \(Cloud\).*private-cloud \(Cloud\)/);
 assert.doesNotMatch(named(f.root, 'provider_0').textContent, /Ollama|browser-tool/);
});


test('route startup follows the selected provider and stays off cloud destinations', async t => {
 const f = page(t);
 const local = { key: 'vllm', name: 'vLLM', kind: 'on-demand', protocol: 'openai' };
 const external = { key: 'external-api', name: 'External API', kind: 'external', protocol: 'openai' };
 const choices = { ...options, catalog: [...catalog, local, external], nodes: [
  { name: 'worker', health_status: 'healthy', providers: [
   { key: 'vllm', run_lifecycle: 'process' }, { key: 'external-api', run_lifecycle: 'none' }, { key: 'groq', kind: 'cloud' },
  ] },
 ] };
 const original = { ...route.replicas[0], provider: 'vllm', on_demand: true };
 f.root.append(routeForm(f.p, { ...route, replicas: [original] }, choices, () => {}));
 const provider = named(f.root, 'provider_0');
 const checkbox = named(f.root, 'on_demand_0');
 const startup = find(f.root, node => node.tag === 'label' && node.children.includes(checkbox));
 assert.equal(startup.hidden, false);
 assert.equal(checkbox.checked, true);
 assert.deepEqual(provider.children.filter(item => item.tag === 'optgroup').map(item => item.getAttribute('label')),
  ['Local providers', 'Cloud providers']);
 const choose = value => { provider.value = value; provider.dispatchEvent(new Event('change')); };
 choose('external-api');
 assert.equal(startup.hidden, true);
 choose('groq');
 assert.equal(startup.hidden, true);
 assert.equal(checkbox.disabled, true);
 choose('vllm');
 assert.equal(startup.hidden, false);
 assert.equal(checkbox.checked, true);
 choose('groq');
 await submit(editor(f.root));
 assert.deepEqual(f.calls[0].body.replicas, [{ ...original, provider: 'groq', on_demand: false }]);
});

test('an unchanged cloud destination keeps its saved settings with startup hidden', async t => {
 const f = page(t);
 const legacy = { ...route.replicas[0], on_demand: true };
 f.root.append(routeForm(f.p, { ...route, replicas: [legacy] }, options, () => {}));
 assert.equal(named(f.root, 'on_demand_0').disabled, true);
 named(f.root, 'route_description').value = 'Renamed';
 await submit(editor(f.root));
 assert.deepEqual(f.calls[0].body, { description: 'Renamed' });
});

test('an old conflict recovery read cannot replace a newer editor in the same panel', async t => {
 const f = page(t, async (_path, opts) => { if (opts.method === 'PATCH') throw Object.assign(new Error('Concurrent change'), { status: 412 }); });
 await routes(f.p, 'coding'); button(f.root, 'Edit').dispatchEvent(new Event('click'));
 named(f.root, 'route_description').value = 'First draft'; await submit(editor(f.root));
 let release; const held = new Promise(resolve => { release = resolve; });
 const list = f.p.api.list; f.p.api.list = path => path === '/model-groups' ? held : list(path);
 button(f.root, 'Reload routes').dispatchEvent(new Event('click')); await drain();
 button(f.root, 'Cancel').dispatchEvent(new Event('click')); button(f.root, 'Edit').dispatchEvent(new Event('click'));
 const current = editor(f.root); named(f.root, 'route_description').value = 'New draft'; current.dispatchEvent(new Event('input'));
 release([structuredClone(route)]); await drain();
 assert.ok(f.root.contains(current)); assert.equal(named(f.root, 'route_description').value, 'New draft');
});

test('Refresh reloads optional route picker inventory and a route named new remains a record', async t => {
 const f = page(t); let expanded = false;
 const list = f.p.api.list;
 f.p.api.list = path => {
  if (expanded && path.startsWith('/providers/catalog')) return [...catalog, { key: 'new-engine', name: 'New engine', protocol: 'openai' }];
  if (expanded && path === '/nodes') return [{ name: 'worker', health_status: 'healthy', providers: [{ key: 'new-engine', state: 'live' }] }];
  return list(path);
 };
 await routes(f.p, 'new');
 assert.ok(button(f.root, 'Edit')); assert.equal(walkForms(f.root, 'route-editor').length, 0);
 expanded = true; button(f.root, 'Refresh').dispatchEvent(new Event('click')); await drain();
 button(f.root, 'Edit').dispatchEvent(new Event('click'));
 assert.match(named(f.root, 'provider_0').textContent, /New engine/);
});
function walkForms(root, cls) { return [root, ...root.children.flatMap(child => walkForms(child, cls))].filter(node => node.tag === 'form' && node.className?.includes(cls)); }
