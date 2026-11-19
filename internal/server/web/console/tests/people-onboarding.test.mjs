import test from 'node:test';
import assert from 'node:assert/strict';
import { signIn, takeAccessLink, takeAuthorizationLink, validNewPassword } from '../login.mjs';
import { people, accountSettings } from '../people.mjs';
import { onboarding } from '../onboarding.mjs';
import { parsePairingTransfer } from '../pairing-transfer.mjs';
import { addNode, approveNode } from '../nodes.mjs';
import { installDOM, named, button, find, walk, drain } from './dom-fixture.mjs';

const submit = async root => { await find(root, node => node.tag === 'form').listeners.get('submit')({ preventDefault() {} }); await drain(); };
const click = async (root, label) => { await button(root, label).listeners.get('click')(); await drain(); };
const expiry = '2030-01-01T00:00:00Z';

function fixture(t, handler = async () => ({})) {
  const root = installDOM(t);
  const controller = new AbortController();
  t.after(() => controller.abort());
  const calls = [], routes = [];
  let signed = 0, refreshed = 0;
  const p = { root, scope: { signal: controller.signal }, state: { issuer: 'https://router.example', node_name: 'Atlas' }, identity: { id: 'owner', kind: 'person', owner: true, role: 'admin' }, current: () => !controller.signal.aborted,
    signedIn: async () => signed++, refresh: async () => refreshed++, syncActivity() {}, go: (...args) => routes.push(args), reload: async () => refreshed++,
    api: { canLeave: () => true, canChange: () => true, data: async (path, options = {}) => { calls.push({ path, ...options }); return handler(path, options); }, list: async path => handler(path, {}), request: async (path, options = {}) => { calls.push({ path, ...options }); return handler(path, options); } } };
  return { p, root, calls, routes, controller, signed: () => signed, refreshed: () => refreshed };
}

test('invitation fragments are removed synchronously and never treated as ordinary navigation', () => {
  const changes = [];
  const history = { state: { example: 1 }, replaceState: (...args) => changes.push(args) };
  assert.deepEqual(takeAccessLink({ hash: '#invite?token=synthetic%2Btoken', pathname: '/console/', search: '?safe=1' }, history), { purpose: 'invitation', token: 'synthetic+token' });
  assert.deepEqual(changes, [[history.state, '', '/console/?safe=1']]);
  assert.equal(takeAccessLink({ hash: '#invite?token=' }, history), null);
  assert.equal(takeAccessLink({ hash: '#overview?item=node' }, history), null);
  assert.equal(changes.length, 2);
});

test('password bounds count Unicode characters without trimming passphrases', () => {
  assert.equal(validNewPassword('🧭'.repeat(12)), true);
  assert.equal(validNewPassword('🧭'.repeat(11)), false);
  assert.equal(validNewPassword('a'.repeat(129)), false);
  assert.equal(validNewPassword(' a passphrase '), true);
});

test('OAuth continuation is scrubbed synchronously and accepts only the relative consent endpoint', () => {
  const changes = [];
  const history = { state: { example: 1 }, replaceState: (...args) => changes.push(args) };
  const target = '/oauth/authorize?client_id=editor&state=synthetic%2Bstate&redirect_uri=http%3A%2F%2Flocalhost%3A7312%2Fcallback';
  const location = { origin: 'https://router.example', pathname: '/console/', search: '', hash: '#authorize?request=' + encodeURIComponent(target) };
  assert.equal(takeAuthorizationLink(location, history), target);
  assert.deepEqual(changes, [[history.state, '', '/console/']]);
  assert.equal(JSON.stringify(history.state).includes('synthetic'), false);
  assert.equal(takeAuthorizationLink({ ...location, hash: '#connections' }, history), null);
  assert.equal(changes.length, 1);
  for (const request of [
    'https://attacker.example/oauth/authorize?client_id=bad', '//attacker.example/oauth/authorize?client_id=bad',
    'https://router.example/oauth/authorize?client_id=editor', 'javascript:alert(1)',
    '/oauth/connections?next=/oauth/authorize', '/oauth/authorize/../connections?client_id=editor',
    '/oauth/authorize?client_id=editor#attacker', '/oauth/authorize?client_id=editor\\attacker',
    '/oauth/authorize?client_id=editor\nattacker', '/oauth/authorize?', '/oauth/authorize?state=' + 'x'.repeat(16384),
  ]) {
    assert.equal(takeAuthorizationLink({ ...location, hash: '#authorize?request=' + encodeURIComponent(request) }, history), null, request.slice(0, 100));
    assert.deepEqual(changes.at(-1), [history.state, '', '/console/']);
  }
  assert.equal(takeAuthorizationLink({ ...location, hash: '#authorize?request=' + encodeURIComponent(target) + '&request=' + encodeURIComponent(target) }, history), null);
});

test('account login uses password autocomplete, keeps spaces, and clears the password before sending', async t => {
  const f = fixture(t, async path => path === '/access/setup' ? { initialized: true } : {});
  await signIn(f.p);
  assert.equal(named(f.root, 'username').getAttribute('autocomplete'), 'username');
  const password = named(f.root, 'password');
  assert.equal(password.getAttribute('autocomplete'), 'current-password');
  named(f.root, 'username').value = 'alice'; password.value = ' secret passphrase ';
  await submit(f.root);
  assert.equal(password.value, '');
  assert.deepEqual(f.calls.at(-1).body, { username: 'alice', password: ' secret passphrase ' });
  assert.equal(f.calls.at(-1).path, '/session/password');
  assert.equal(f.calls.at(-1).notifyAuth, false);
  assert.equal(f.signed(), 1);
  assert.equal(walk(f.root).some(node => node.getAttribute('name') === 'key'), false, 'key login is an explicit secondary choice');
});

test('owner recovery keeps public CLI guidance through code entry and uses the bound recovery API', async t => {
  const f = fixture(t, async path => path === '/access/setup' ? { initialized: true } : { purpose: 'recovery', token: 'bound-recovery', expires_at: expiry });
  await signIn(f.p);
  await click(f.root, 'Get help signing in');
  assert.match(f.root.textContent, /zzrouter access recover-owner/);
  await click(f.root, 'Enter recovery code');
  assert.match(f.root.textContent, /zzrouter access recover-owner/);
  assert.doesNotMatch(f.root.textContent, /zzrouter console|zzrouter-node/);
  named(f.root, 'setup_code').value = 'temporary-recovery';
  await submit(f.root);
  assert.deepEqual(f.calls.at(-1).body, { token: 'temporary-recovery' });
  assert.equal(f.calls.at(-1).path, '/access/console-grants/exchange');
  named(f.root, 'password').value = 'new owner passphrase';
  await submit(f.root);
  assert.equal(f.calls.at(-1).path, '/access/recovery');
  assert.deepEqual(f.calls.at(-1).body, { token: 'bound-recovery', password: 'new owner passphrase' });
  assert.equal(f.signed(), 1);
});

test('first owner form is available only after a temporary code exchange and uses its bound token', async t => {
  const f = fixture(t, async path => path === '/access/setup' ? { initialized: false } : { purpose: 'setup', token: 'bound-token', expires_at: expiry });
  await signIn(f.p);
  assert.match(f.root.textContent, /Set up your router/);
  assert.equal(walk(f.root).some(node => node.getAttribute('name') === 'username'), false);
  named(f.root, 'setup_code').value = 'temporary-token';
  await submit(f.root);
  assert.equal(f.calls.at(-1).path, '/access/console-grants/exchange');
  assert.deepEqual(f.calls.at(-1).body, { token: 'temporary-token' });
  named(f.root, 'username').value = 'owner'; named(f.root, 'name').value = 'Owner'; named(f.root, 'password').value = 'long secret passphrase';
  await submit(f.root);
  assert.deepEqual(f.calls.at(-1).body, { token: 'bound-token', username: 'owner', name: 'Owner', password: 'long secret passphrase' });
  assert.equal(f.signed(), 1);
});

for (const purpose of ['invitation', 'recovery']) test(`${purpose} link is reviewed before exchange and posts only the matching operation`, async t => {
  const f = fixture(t, async path => path === '/access/invitations/inspect' ? { purpose, team_id: 'engineering', role: 'user', expires_at: expiry } : { purpose, token: 'bound-token', expires_at: expiry });
  await signIn(f.p, { purpose, token: 'link-token' });
  assert.deepEqual(f.calls.map(call => call.path), ['/access/invitations/inspect']);
  assert.doesNotMatch(f.root.textContent, /link-token/);
  assert.doesNotMatch(f.root.textContent, /\bnull\b/);
  await click(f.root, 'Continue');
  named(f.root, 'password').value = 'new long passphrase';
  if (purpose === 'invitation') { named(f.root, 'username').value = 'alice'; named(f.root, 'name').value = 'Alice'; }
  await submit(f.root);
  assert.equal(f.calls.at(-1).path, purpose === 'invitation' ? '/access/invitations/accept' : '/access/recovery');
  assert.equal(f.calls.at(-1).body.token, 'bound-token');
  assert.equal(f.signed(), 1);
});

test('uncertain account creation blocks replay and offers ordinary sign-in', async t => {
  const f = fixture(t, async path => {
    if (path === '/access/setup') return { initialized: false };
    if (path === '/access/console-grants/exchange') return { purpose: 'invitation', token: 'bound-token' };
    throw Object.assign(new Error('Response lost'), { uncertain: true });
  });
  await signIn(f.p); named(f.root, 'setup_code').value = 'token'; await submit(f.root);
  named(f.root, 'username').value = 'alice'; named(f.root, 'name').value = 'Alice'; named(f.root, 'password').value = 'long secret passphrase';
  await submit(f.root);
  assert.match(f.root.textContent, /account may already be saved/);
  await submit(f.root);
  assert.equal(f.calls.filter(call => call.path === '/access/invitations/accept').length, 1);
  f.controller.abort();
  assert.equal(named(f.root, 'password').value, '');
});

test('onboarding saves navigation choices but cannot manufacture a verified response', async t => {
  const f = fixture(t, async () => ({ step: '', dismissed: false, provider_ready: false, model_ready: false, response_verified: false, app_connected: false }));
  await onboarding(f.p);
  assert.match(f.root.textContent, /Prepare a model/);
  assert.match(f.root.textContent, /Test in Chat/);
  const next = walk(f.root).find(node => node.tag === 'button' && node.textContent === 'Choose provider');
  await next.listeners.get('click')();
  assert.deepEqual(f.calls.at(-1).body, { step: 'provider' });
  assert.deepEqual(f.routes.at(-1), ['providers', undefined]);
  await click(f.root, 'Skip setup for now');
  assert.deepEqual(f.calls.at(-1).body, { dismissed: true });
  assert.ok(f.calls.filter(call => call.method === 'PATCH').every(call => Object.keys(call.body).every(key => ['step', 'dismissed'].includes(key))));
});

test('onboarding shows API availability separately from a successful response', async t => {
  const progress = { provider_ready: true, model_ready: true, response_verified: false, app_connected: false };
  const f = fixture(t, async () => progress);
  await onboarding(f.p);
  const row = title => find(f.root, node => node.tag === 'li' && node.textContent.startsWith(title));
  assert.match(row('Connect a provider').textContent, /Available/);
  assert.match(row('Prepare a model').textContent, /Available/);
  assert.doesNotMatch(row('Test in Chat').textContent, /Done/);
  await click(f.root, 'Open Chat');
  assert.deepEqual(f.calls.at(-1).body, { step: 'test' });
  assert.deepEqual(f.routes.at(-1), ['chat', undefined]);
  Object.assign(progress, { provider_ready: false, model_ready: false, response_verified: true });
  await click(f.root, 'Check progress');
  assert.equal(f.root.textContent, '');
  assert.equal(f.calls.filter(call => call.method === 'PATCH').length, 1, 'completion needs no dismissal write');
});

test('completed setup leaves the whole dashboard space for the map', async t => {
  for (const appConnected of [false, true]) {
    const f = fixture(t, async () => ({ response_verified: true, app_connected: appConnected }));
    await onboarding(f.p);
    assert.equal(f.root.children.length, 0);
    assert.equal(f.calls.filter(call => call.method === 'PATCH').length, 0);
  }
});

test('saved navigation does not establish model readiness and non-owners get no setup content', async t => {
  const f = fixture(t, async () => ({ step: 'model', provider_ready: true, model_ready: false }));
  await onboarding(f.p);
  const next = walk(f.root).find(node => node.tag === 'button' && node.textContent === 'Choose model');
  await next.listeners.get('click')();
  assert.deepEqual(f.routes.at(-1), ['models', undefined]);
  f.root.replaceChildren(); f.calls.length = 0; f.p.identity.owner = false;
  await onboarding(f.p);
  assert.deepEqual(f.calls, []);
  assert.equal(f.root.textContent, '');
  assert.doesNotMatch(f.root.textContent, /\bnull\b/);
});

test('People uses an editable details panel and invitations preview inherited team policy', async t => {
  const record = { id: 'alice', name: 'Alice', username: 'alice', role: 'user', team_id: 'engineering', suspended: false };
  const team = { id: 'engineering', name: 'Engineering', kind: 'shared', provider_access: { mode: 'selected', providers: ['ollama'] }, route_access: { mode: 'all' } };
  const f = fixture(t, async (path, options) => {
    if (path === '/access/people') return [record];
    if (path === '/teams') return [team];
    if (path === '/access/invitations' && !options.method) return [];
    if (path === '/access/invitations') return { purpose: 'invitation', token: 'single-use-secret', team_id: team.id, expires_at: expiry };
    return record;
  });
  let select;
  f.p.handleSelection = handler => { select = handler; };
  f.p.go = (_page, item, mode) => select(item, mode);
  await people(f.p);
  await click(f.root, 'Alice');
  assert.doesNotMatch(f.root.textContent, /\bnull\b/);
  assert.equal(walk(f.root).filter(node => node.tag === 'dialog').length, 1);
  await click(f.root, 'Edit');
  named(f.root, 'name').value = 'Alice updated';
  await submit(f.root);
  assert.equal(f.calls.find(call => call.method === 'PATCH').body.name, 'Alice updated');
  await select(undefined, 'create');
  assert.match(f.root.textContent, /Inherited team access/);
  await submit(f.root);
  assert.equal(f.calls.at(-1).path, '/access/invitations');
  assert.deepEqual(f.calls.at(-1).body, { team_id: 'engineering', role: 'user', expires_in_hours: 24 });
  assert.match(f.root.textContent, /Copy this link now/);
  f.controller.abort();
  assert.equal(walk(f.root).some(node => node.value?.includes('single-use-secret')), false);
});

test('account password changes submit only the existing API and clear both fields', async t => {
  const f = fixture(t);
  f.root.append(accountSettings(f.p));
  const current = named(f.root, 'current_password'), next = named(f.root, 'password');
  current.value = 'old passphrase'; next.value = 'new passphrase';
  await submit(f.root);
  assert.equal(f.calls.at(-1).path, '/access/password');
  assert.equal(current.value, ''); assert.equal(next.value, '');
  assert.equal(f.refreshed(), 1);
});

test('pairing documents are bounded and require explicit review before approval', async t => {
  const transfer = { type: 'zzrouter-pairing', version: 1, node_name: 'worker', code: 'synthetic-code', worker_url: 'https://worker.example:9091', worker_fingerprint: 'sha256:synthetic', expires_at: expiry };
  assert.deepEqual(parsePairingTransfer(JSON.stringify(transfer)), transfer);
  for (const value of ['[]', '{}', 'x'.repeat(8193), JSON.stringify({ ...transfer, script: 'must-not-run' })]) assert.throws(() => parsePairingTransfer(value));
  const f = fixture(t, async path => path === '/nodes' ? [] : { node_name: 'worker' });
  f.p.identity.owner = false;
  approveNode(f.p); named(f.root, 'pairing_transfer').value = JSON.stringify(transfer);
  await submit(f.root);
  assert.equal(f.calls.length, 0);
  await click(f.root, 'Review computer');
  assert.match(f.root.textContent, /sha256:synthetic/);
  assert.doesNotMatch(f.root.textContent, /synthetic-code/);
  await submit(f.root);
  assert.deepEqual(f.calls[0].body, { transfer });
  assert.equal(named(f.root, 'pairing_transfer').value, '');
});

test('a late pairing file read cannot overwrite newer pasted details', async t => {
  const f = fixture(t);
  approveNode(f.p);
  let complete;
  const upload = named(f.root, 'pairing_file');
  upload.files = [{ size: 100, text: () => new Promise(resolve => { complete = resolve; }) }];
  const reading = upload.listeners.get('change')();
  const transfer = named(f.root, 'pairing_transfer');
  transfer.value = 'newer pasted details';
  transfer.dispatchEvent(new Event('input'));
  complete('older file content');
  await reading;
  assert.equal(transfer.value, 'newer pasted details');
});

test('worker guidance is read-only and renders API instructions for the selected operating system', async t => {
  const f = fixture(t, async () => ({ coordinator_url: 'https://coord.example:443', ca_fingerprint: 'sha256:public', worker_port: 9191, version: '1.2.3', download_url: 'https://github.com/stperic/zzrouter/releases', platforms: [
    { os: 'linux', label: 'Linux', archives: 'linux-amd64', installations: [{ id: 'service', label: 'System service', shell: 'Terminal', startup: 'Starts at boot', install_command: 'synthetic install --role worker --cluster-port 9191', pair_command: 'synthetic pair --secure' }] },
    { os: 'windows', label: 'Windows', archives: 'windows-amd64', installations: [{ id: 'service', label: 'All users', shell: 'Administrator PowerShell', install_command: 'install-service', pair_command: 'service-pair', pair_shell: 'Open a new administrator PowerShell' }, { id: 'user', label: 'This account', shell: 'PowerShell', install_command: 'install-user', pair_command: 'user-pair', pair_shell: 'Open a new PowerShell' }] },
  ] }));
  await addNode(f.p);
  assert.deepEqual(f.calls.map(call => [call.path, call.method]), [['/cluster/pairing/setup', undefined]]);
  named(f.root, 'worker_os').value = 'linux';
  named(f.root, 'worker_os').dispatchEvent(new Event('change'));
  assert.match(f.root.textContent, /On the new computer/);
  assert.match(f.root.textContent, /Back in this console/);
  assert.ok(walk(f.root).some(node => node.value === 'synthetic pair --secure'));
  named(f.root, 'worker_os').value = 'windows';
  named(f.root, 'worker_os').dispatchEvent(new Event('change'));
  assert.ok(walk(f.root).some(node => node.value === 'service-pair'));
  named(f.root, 'worker_installation').value = 'user';
  named(f.root, 'worker_installation').dispatchEvent(new Event('change'));
  assert.ok(walk(f.root).some(node => node.value === 'user-pair'));
  assert.equal(walk(f.root).some(node => node.value === 'service-pair'), false);
  await click(f.root, 'I have pairing details');
  assert.ok(named(f.root, 'pairing_transfer'));
  assert.equal(f.calls.length, 1, 'viewing instructions does not create enrollment');
});

test('a definitive expired pairing refusal allows replacement without locking the panel', async t => {
  const f = fixture(t, async () => { throw Object.assign(new Error('Pairing expired. Generate new details.'), { status: 400 }); });
  approveNode(f.p);
  const transfer = { type: 'zzrouter-pairing', version: 1, node_name: 'worker', code: 'expired-code', worker_url: 'https://worker.example:9091', worker_fingerprint: 'sha256:synthetic', expires_at: '2020-01-01T00:00:00Z' };
  named(f.root, 'pairing_transfer').value = JSON.stringify(transfer);
  await click(f.root, 'Review computer');
  await submit(f.root);
  assert.match(f.root.textContent, /Pairing expired/);
  assert.doesNotMatch(f.root.textContent, /Enrollment may already have completed/);
  assert.equal(walk(f.root).find(node => node.tag === 'fieldset').disabled, false);
  assert.equal(named(f.root, 'pairing_transfer').value, '');
  named(f.root, 'pairing_transfer').value = JSON.stringify({ ...transfer, code: 'new-code', expires_at: expiry });
  named(f.root, 'pairing_transfer').dispatchEvent(new Event('input'));
  await click(f.root, 'Review computer');
  assert.equal(button(f.root, 'Approve computer').disabled, false);
});

test('manual setup progress refresh failures remain recoverable without an unhandled rejection', async t => {
  let unavailable = false;
  const f = fixture(t, async () => { if (unavailable) throw new Error('Progress unavailable'); return {}; });
  await onboarding(f.p);
  unavailable = true;
  await click(f.root, 'Check progress');
  assert.match(f.root.textContent, /Setup progress is unavailable/);
  unavailable = false;
  await click(f.root, 'Retry');
  assert.match(f.root.textContent, /Get your first model working/);
});
