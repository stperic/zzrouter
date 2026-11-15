import test, { before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { chromium } from 'playwright';

let browser, artifacts;
before(async () => {
  browser = await chromium.launch({ headless: true, executablePath: process.env.CONSOLE_CHROME || undefined });
  artifacts = await mkdtemp(join(tmpdir(), 'zzrouter-console-ux-'));
  console.log('Browser evidence: ' + artifacts);
});
after(async () => { await browser?.close(); });

async function consolePage(t, scenario = 'populated') {
  const fixture = spawn(process.execPath, [fileURLToPath(new URL('./fixture.mjs', import.meta.url)), '0'], { stdio: ['ignore', 'pipe', 'inherit'] });
  const base = await new Promise((resolve, reject) => { fixture.once('error', reject); fixture.stdout.once('data', data => resolve(data.toString().trim())); });
  const origin = new URL(base).origin;
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await context.addInitScript({ path: fileURLToPath(new URL('../node_modules/axe-core/axe.min.js', import.meta.url)) });
  const page = await context.newPage();
  const errors = [], dialogs = [], answers = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('dialog', dialog => { dialogs.push(dialog.message()); void (answers.shift() === 'dismiss' ? dialog.dismiss() : dialog.accept()); });
  t.after(async () => { await context.close(); fixture.kill(); assert.deepEqual(errors, [], 'No unhandled browser errors'); });
  const state = async value => { await fetch(origin + '/__scenario?state=' + value); };
  await state(scenario);
  let navigation = 0;
  const open = async (hash, width = 1440, height = 900) => {
    await page.setViewportSize({ width, height }); await page.goto(base + '?test=' + ++navigation + hash);
    await page.locator('#content h1').waitFor({ state: 'attached' });
    if (hash.startsWith('#chat')) await page.getByRole('textbox', { name: 'Message', exact: true }).waitFor();
    if (hash.startsWith('#overview')) await page.locator('.topology-status:empty').waitFor({ state: 'attached' });
  };
  const nav = async label => {
    const navigation = page.getByRole('navigation', { name: 'Main navigation' });
    if (!await navigation.isVisible()) await page.getByRole('button', { name: 'Navigation', exact: true }).click();
    await navigation.getByRole('button', { name: label, exact: true }).click();
  };
  const model = async id => {
    await page.getByRole('button', { name: /^(Choose model|Change)$/ }).click();
    await page.getByRole('searchbox', { name: 'Find a model or route' }).fill(id);
    await page.locator('.model-result').filter({ hasText: id }).first().click();
  };
  return { page, base, origin, state, open, nav, model, dialogs, answers,
    requests: async () => (await fetch(origin + '/__requests')).json() };
}

async function audit(page, name) {
  const geometry = await page.evaluate(() => {
    const bounds = selector => [...document.querySelectorAll(selector)].filter(node => node.getClientRects().length).map(node => {
      const r = node.getBoundingClientRect(); return { label: node.getAttribute('aria-label') || node.className, top: r.top, bottom: r.bottom, left: r.left, right: r.right };
    });
    return { width: innerWidth, height: innerHeight, documentWidth: document.documentElement.scrollWidth,
      controls: bounds('.detail-dismiss,.editor-actions,.chat-composer'), panels: bounds('dialog[open]') };
  });
  const violations = await page.evaluate(async () => (await axe.run(document, {
    runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'] },
  })).violations.map(value => ({ id: value.id, nodes: value.nodes.map(node => ({ target: node.target, message: node.failureSummary })) })));
  await page.screenshot({ path: join(artifacts, name + '.png'), fullPage: true });
  await writeFile(join(artifacts, name + '.json'), JSON.stringify({ geometry, violations }, null, 2));
  assert.ok(geometry.documentWidth <= geometry.width + 1, name + ': page must not overflow horizontally');
  assert.doesNotMatch(await page.locator('body').innerText(), /\[object (?:HTML|Object)/, name + ': DOM children must render as elements');
  for (const box of [...geometry.controls, ...geometry.panels]) {
    assert.ok(box.top >= -1 && box.bottom <= geometry.height + 1, name + ': visible controls within viewport: ' + JSON.stringify(box));
    assert.ok(box.left >= -1 && box.right <= geometry.width + 1, name + ': panel must fit width');
  }
  assert.deepEqual(violations, [], name + ': accessibility violations');
}

test('all console views and shared editors fit desktop and phone viewports', { timeout: 180000 }, async t => {
  const f = await consolePage(t);
  const scenes = [
    ['overview', '#overview'], ['nodes', '#nodes'], ['node-editor', '#nodes?item=node%2Fstudio-gpu', 'Edit'],
    ['add-node', '#nodes?item=add'], ['providers', '#providers'], ['local-provider', '#providers?item=local%2Follama'],
    ['cloud-editor', '#providers?item=cloud%2Fgroq', 'Edit'], ['routes', '#routes'], ['route-editor', '#routes?item=coding', 'Edit'],
    ['create-route', '#routes?mode=create'], ['clients', '#clients'], ['keys', '#access'], ['key-editor', '#access?item=keys%2Feditor', 'Edit'],
    ['team-editor', '#access?item=teams%2Fengineering', 'Edit'], ['create-team', '#access?item=teams&mode=create'],
    ['workloads', '#workloads'], ['jobs', '#workloads?mode=jobs'], ['system', '#settings'], ['chat', '#chat'],
  ];
  for (const width of [1280, 390]) for (const [name, hash, action] of scenes) {
    await f.open(hash, width, width === 390 ? 844 : 800);
    if (action) await f.page.getByRole('dialog').getByRole('button', { name: action, exact: true }).click();
    await audit(f.page, name + '-' + width);
  }
  await f.state('user'); await f.open('#connections', 390, 844); await audit(f.page, 'my-access-user');
  await f.state('login'); await f.page.goto(f.base + '#chat');
  await f.page.getByRole('button', { name: 'Sign in', exact: true }).waitFor();
  await audit(f.page, 'sign-in');
});

test('complete console workspaces and editors fit desktop and phone', { timeout: 180000 }, async t => {
  const f = await consolePage(t);
  const scenes = [
    ['models', '#models'], ['model-details', '#models?item=studio-gpu%2Fvllm%2F%2Fqwen3-coder'],
    ['discovery', '#models?mode=discover'], ['downloads', '#workloads?mode=downloads'],
    ['download-details', '#workloads?mode=downloads&item=model-download'],
    ['logs', '#logs'], ['request-details', '#logs?item=request-one'],
    ['remote-provider', '#providers?item=local&mode=create'],
    ['setup', '#settings?item=setup'], ['software', '#settings?item=updates'],
    ['node-software', '#settings?item=updates%2Fstudio-gpu'], ['install-release', '#settings?item=updates&mode=create'],
    ['pricing', '#settings?item=pricing'], ['create-price', '#settings?item=pricing&mode=create'],
    ['edit-price', '#settings?item=pricing%2Fgroq%2Fllama-3.3-70b-versatile', 'Edit'],
  ];
  for (const width of [1280, 390]) for (const [name, hash, action] of scenes) {
    await f.open(hash, width, width === 390 ? 844 : 800);
    if (action) await f.page.getByRole('dialog').getByRole('button', { name: action, exact: true }).click();
    await audit(f.page, name + '-' + width);
  }
});

test('model discovery reviews exact targets, downloads, test and Chat handoff', { timeout: 60000 }, async t => {
  const f = await consolePage(t); const { page } = f;
  await f.open('#models');
  await page.getByRole('button', { name: 'qwen3-coder', exact: true }).click();
  await page.getByRole('button', { name: 'Test model', exact: true }).click();
  await page.getByRole('button', { name: 'Send test', exact: true }).click();
  await page.getByText('studio-gpu', { exact: true }).last().waitFor();
  await f.open('#models?item=studio-gpu%2Fvllm%2F%2Fqwen3-coder');
  await page.getByRole('dialog').getByRole('button', { name: 'Chat', exact: true }).click();
  await page.getByRole('textbox', { name: 'Message', exact: true }).waitFor();
  assert.match(await page.locator('.chat-settings-summary').innerText(), /Node: studio-gpu/);
  await f.open('#models?mode=discover');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await page.getByRole('button', { name: /^Small model/ }).click();
  await page.getByRole('button', { name: /Download|Add model/ }).click();
  await page.getByLabel('Local provider', { exact: true }).selectOption('vllm');
  await page.getByLabel('Weights variant', { exact: true }).selectOption('small-Q4.gguf');
  await page.getByLabel('studio-gpu', { exact: true }).check();
  await page.getByRole('button', { name: 'Review download', exact: true }).click();
  await audit(page, 'download-review');
  await page.getByRole('button', { name: 'Start download', exact: true }).click();
  await page.getByText('model-download', { exact: true }).waitFor();
  const writes = (await f.requests()).filter(row => row.path === '/deployments' && row.method === 'POST');
  assert.equal(writes.length, 1);
  assert.deepEqual(writes[0].payload, { model: 'example/small-model', registry: 'huggingface', provider: 'vllm', nodes: ['studio-gpu'], file: 'small-Q4.gguf' });
});

test('provider plans, parameter scopes and services use the shared side panel', { timeout: 60000 }, async t => {
  const f = await consolePage(t); const { page } = f;
  const manage = async () => {
    await f.open('#providers?item=local%2Fvllm', 390, 844);
    await page.getByRole('dialog').getByLabel('Compute node', { exact: true }).selectOption('studio-gpu');
    await page.getByRole('button', { name: 'Manage runtime', exact: true }).click();
  };
  await manage(); await page.getByRole('button', { name: 'Parameters', exact: true }).click();
  await page.locator('.parameter-setting').first().waitFor();
  await audit(page, 'parameter-editor-phone');
  assert.equal(await page.getByText('synthetic-private-value', { exact: true }).count(), 0);
  await page.locator('input[name="parameter_node"]').fill('office-pc');
  await page.getByRole('button', { name: 'View scope', exact: true }).click();
  await page.locator('.parameter-setting').first().waitFor();
  assert.ok((await f.requests()).some(row => row.path.endsWith('/parameters') && row.query.scope === 'node' && row.query.node === 'office-pc'));
  await manage(); await page.getByRole('button', { name: 'Install or repair', exact: true }).click();
  await page.getByLabel('Runtime', { exact: true }).selectOption('python');
  await page.getByRole('button', { name: 'Review install plan', exact: true }).click();
  await page.getByRole('button', { name: 'Run all steps', exact: true }).waitFor();
  await audit(page, 'install-plan-phone');
  await page.getByRole('button', { name: 'Verify step', exact: true }).click();
  await page.getByText('Synthetic verification passed', { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Run all steps', exact: true }).click();
  await page.getByRole('heading', { name: 'install-job', exact: true }).waitFor();
  const install = (await f.requests()).filter(row => row.path === '/providers/vllm/install' && row.method === 'POST');
  assert.equal(install.length, 1);
  assert.deepEqual(install[0].payload, { expected_plan_id: 'plan-1', version: '1.0.0', runtime: 'python', action: 'install', components: [] });
  assert.equal(install[0].query.node, 'studio-gpu');
  await manage(); await page.getByRole('button', { name: 'Service', exact: true }).click();
  await audit(page, 'provider-service-phone');
  await manage(); await page.getByRole('button', { name: 'Versions', exact: true }).click();
  await page.getByLabel('Cluster version pin').waitFor();
  await audit(page, 'provider-versions-phone');
});

test('runtime replay, inference payload and management writes remain deliberate', { timeout: 60000 }, async t => {
  const f = await consolePage(t); const { page } = f;
  await f.open('#workloads');
  await page.getByRole('button', { name: 'qwen3-coder', exact: true }).click();
  await page.getByRole('button', { name: 'Runtime logs', exact: true }).click();
  await page.getByText('old tail', { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Follow live', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('.log-output')?.textContent === 'repeated line\nrepeated line');
  assert.equal(await page.locator('.log-output').innerText(), 'repeated line\nrepeated line');
  await page.getByRole('button', { name: 'Follow live', exact: true }).click();
  await page.getByText(/The log stream ended/).waitFor();
  assert.equal(await page.locator('.log-output').innerText(), 'repeated line\nrepeated line');
  await audit(page, 'runtime-logs');
  await f.open('#logs?item=request-one');
  await page.getByRole('button', { name: 'Show retained payload', exact: true }).click();
  await page.getByText(/Private fixture prompt/).waitFor();
  await page.getByRole('button', { name: 'Hide payload', exact: true }).click();
  assert.equal(await page.getByText(/Private fixture prompt/).count(), 0);
  await page.getByRole('button', { name: 'Close request details', exact: true }).click();
  await page.getByRole('button', { name: 'Follow live', exact: true }).click();
  await page.getByText(/Some live observations were dropped/).waitFor();
  await f.open('#settings?item=updates%2Fstudio-gpu');
  await page.getByRole('button', { name: 'Edit update schedule', exact: true }).click();
  await page.getByLabel('Enable scheduled software updates').check();
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByText('Update schedule saved.', { exact: true }).waitFor();
  assert.ok((await f.requests()).some(row => row.path === '/update/settings' && row.method === 'PATCH' && row.query.node === 'studio-gpu' && row.payload.enabled));
  await f.open('#settings?item=pricing%2Fgroq%2Fllama-3.3-70b-versatile');
  await page.getByRole('dialog').getByRole('button', { name: 'Edit', exact: true }).click();
  await page.getByLabel('Output ($ / 1M tokens)', { exact: true }).fill('1.2');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Edit', exact: true }).waitFor();
  const price = (await f.requests()).find(row => row.path === '/pricing/overrides' && row.method === 'POST');
  assert.equal(price.payload.output_cost_per_1m, 1.2); assert.equal(price.payload.cache_read_cost_per_1m, 0.1);
  await f.state('feature-errors'); await f.open('#logs');
  await page.getByText(/Synthetic observation failure/).waitFor();
  await audit(page, 'logs-error');
});

test('software rollout preserves refused drafts and never repeats an interrupted update', { timeout: 45000 }, async t => {
  const f = await consolePage(t); const { page } = f;
  await f.state('update-invalid'); await f.open('#settings?item=updates&mode=create', 390, 844);
  await page.getByLabel('Release version').fill('invalid-release');
  await page.getByLabel('studio-gpu', { exact: true }).check();
  await page.getByRole('button', { name: 'Install selected version', exact: true }).click();
  await page.getByText(/Unknown release version/).waitFor();
  assert.equal(await page.getByLabel('Release version').inputValue(), 'invalid-release');
  assert.equal(await page.getByLabel('studio-gpu', { exact: true }).isChecked(), true);
  await f.state('update-interrupted');
  await page.getByLabel('Release version').fill('0.2.4');
  await page.getByRole('button', { name: 'Install selected version', exact: true }).click();
  await page.getByRole('button', { name: 'Inspect update state', exact: true }).waitFor();
  assert.match(await page.getByRole('dialog').innerText(), /check whether the change completed/);
  assert.equal(await page.getByRole('button', { name: 'Install selected version', exact: true }).count(), 0);
  await page.getByRole('button', { name: 'Inspect update state', exact: true }).click();
  const sent = (await f.requests()).filter(row => row.path === '/update/apply');
  assert.equal(sent.length, 2);
  assert.deepEqual(sent[1].payload, { version: '0.2.4', nodes: ['studio-gpu'] });
  await audit(page, 'update-interrupted-inspection');
});

test('Chat retains conversation, draft and settings through navigation and history', { timeout: 45000 }, async t => {
  const f = await consolePage(t), p = f.page;
  await f.open('#chat'); await f.model('coding');
  await p.getByRole('button', { name: 'Settings', exact: true }).click();
  await p.getByRole('textbox', { name: 'System prompt' }).fill('Keep this system instruction.');
  await p.getByRole('spinbutton', { name: 'Temperature', exact: true }).fill('0.3');
  await p.getByRole('combobox', { name: 'Compute node' }).selectOption('studio-gpu');
  await p.getByRole('button', { name: 'Apply settings' }).click();
  await p.getByRole('textbox', { name: 'Message', exact: true }).fill('First prompt');
  await p.getByRole('button', { name: 'Send ↑' }).click();
  await p.getByRole('button', { name: 'Regenerate' }).waitFor();
  await p.getByRole('textbox', { name: 'Message', exact: true }).fill('Retained draft');
  await f.nav('Nodes'); await p.goBack();
  await p.getByRole('textbox', { name: 'Message', exact: true }).waitFor();
  assert.equal(await p.getByRole('textbox', { name: 'Message', exact: true }).inputValue(), 'Retained draft');
  assert.match(await p.locator('.chat-settings-summary').innerText(), /Node: studio-gpu/);
  assert.match(await p.locator('.chat-viewport').innerText(), /First prompt/);
  await f.nav('Routes'); await p.getByRole('button', { name: 'local-chat', exact: true }).click();
  await p.getByRole('button', { name: 'Try in Chat', exact: true }).click();
  assert.equal(await p.locator('.chat-model-name').innerText(), 'local-chat');
  assert.doesNotMatch(await p.locator('.chat-settings-summary').innerText(), /Node:/);
  assert.equal(await p.getByRole('textbox', { name: 'Message', exact: true }).inputValue(), 'Retained draft');
  await p.getByRole('button', { name: 'Send ↑' }).click();
  await p.waitForFunction(() => document.querySelectorAll('.chat-assistant').length === 2);
  const calls = (await f.requests()).filter(row => row.path === '/v1/chat/completions');
  assert.equal(calls.length, 2);
  assert.equal(calls[1].payload.model, 'local-chat');
  assert.equal(calls[1].payload.temperature, 0.3);
  assert.deepEqual(calls[1].payload.messages.map(row => row.role), ['system', 'user', 'assistant', 'user']);
  assert.equal(calls[1].payload.messages[0].content, 'Keep this system instruction.');
  await p.getByRole('button', { name: 'New chat', exact: true }).click();
  await p.waitForFunction(() => !document.querySelector('.chat-message'));
  assert.equal(await p.locator('.chat-message').count(), 0);
  assert.equal(await p.getByRole('textbox', { name: 'Message', exact: true }).inputValue(), '');
  assert.equal(await p.locator('.chat-model-name').innerText(), 'Select a model or route');
});

test('picker and mobile navigation preserve dirty settings and restore focus', { timeout: 45000 }, async t => {
  const f = await consolePage(t), p = f.page;
  await f.open('#chat', 390, 844); await f.model('coding');
  await p.getByRole('button', { name: 'Settings', exact: true }).click();
  await p.getByRole('textbox', { name: 'System prompt' }).fill('Unsaved instruction');
  f.answers.push('dismiss'); await f.nav('Routes');
  assert.equal(await p.getByRole('textbox', { name: 'System prompt' }).inputValue(), 'Unsaved instruction');
  await p.keyboard.press('Escape');
  assert.equal(await p.getByRole('button', { name: 'Navigation', exact: true }).getAttribute('aria-expanded'), 'false');
  assert.equal(await p.getByRole('dialog').count(), 1, 'Escape first closes only navigation');
  await p.getByRole('button', { name: 'Apply settings' }).click();
  await p.getByRole('button', { name: 'Change', exact: true }).click();
  const search = p.getByRole('searchbox', { name: 'Find a model or route' });
  await search.fill('vllm');
  assert.equal(await p.locator('.model-result').count(), 1);
  assert.match(await p.locator('.model-result').innerText(), /studio-gpu/);
  await p.keyboard.press('Escape');
  assert.equal(await p.locator('.chat-model-name').innerText(), 'coding', 'Search is uncommitted');
  assert.equal(await p.evaluate(() => document.activeElement.textContent), 'Change');
  await p.getByRole('button', { name: 'Change', exact: true }).click();
  await search.fill('my-exact-model');
  await p.getByRole('button', { name: 'Use exact ID: my-exact-model', exact: true }).click();
  assert.equal(await p.locator('.chat-model-name').innerText(), 'my-exact-model');
  await p.getByRole('button', { name: 'Settings', exact: true }).click();
  assert.equal(await p.getByRole('textbox', { name: 'System prompt' }).inputValue(), 'Unsaved instruction');
});

test('leaving streaming Chat cancels, retains partial output, and authority changes clear it', { timeout: 45000 }, async t => {
  const f = await consolePage(t, 'streaming'), p = f.page;
  await f.open('#chat'); await f.model('coding');
  await p.getByRole('textbox', { name: 'Message', exact: true }).fill('Private prompt');
  await p.getByRole('button', { name: 'Send ↑' }).click();
  await p.getByText('Partial response', { exact: true }).waitFor();
  await f.nav('Nodes'); await f.nav('Chat');
  await p.getByText('Response stopped.', { exact: true }).waitFor();
  assert.equal((await f.requests()).filter(row => row.path === '/v1/chat/completions').length, 1);
  await p.getByRole('button', { name: 'Regenerate' }).click();
  await p.getByRole('button', { name: 'Stop ■' }).waitFor();
  await f.state('unverified'); await p.evaluate(() => window.dispatchEvent(new Event('focus')));
  await p.getByRole('button', { name: 'Verify session' }).waitFor();
  assert.equal(await p.getByRole('button', { name: 'Stop ■' }).isEnabled(), true);
  await p.getByRole('button', { name: 'Stop ■' }).click();
  assert.equal(await p.getByRole('button', { name: 'Regenerate' }).isEnabled(), false);
  await f.state('changed'); await p.getByRole('button', { name: 'Verify session' }).click();
  await p.getByRole('textbox', { name: 'Message', exact: true }).waitFor();
  await p.waitForFunction(() => !document.body.textContent.includes('Private prompt'));
  assert.equal(await p.locator('.chat-message').count(), 0);
  await f.model('coding');
  await p.getByRole('textbox', { name: 'Message', exact: true }).fill('Clear this draft on pagehide');
  await p.evaluate(() => window.dispatchEvent(new Event('pagehide')));
  assert.equal(await p.locator('.chat-canvas').count(), 0);
  await p.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
  await p.getByRole('textbox', { name: 'Message', exact: true }).waitFor();
  assert.equal(await p.getByRole('textbox', { name: 'Message', exact: true }).inputValue(), '');
  await p.getByRole('button', { name: 'Sign out', exact: true }).click();
  await p.getByRole('button', { name: 'Sign in', exact: true }).waitFor();
  assert.equal(await p.locator('.chat-canvas').count(), 0);
});

test('field feedback exposes local and native errors and reveals collapsed inputs', { timeout: 45000 }, async t => {
  const f = await consolePage(t), p = f.page;
  await f.open('#access?item=teams&mode=create', 390, 844);
  await p.getByRole('textbox', { name: 'Name', exact: true }).fill('New team');
  await p.getByRole('combobox', { name: 'Clients may use', exact: true }).selectOption('selected');
  await p.getByRole('button', { name: 'Create team', exact: true }).click();
  const routeMode = p.getByRole('combobox', { name: 'Clients may use', exact: true });
  assert.equal(await routeMode.getAttribute('aria-invalid'), 'true');
  assert.match(await p.evaluate(() => document.activeElement.textContent), /Select at least one route/);
  await p.getByRole('link', { name: 'Select at least one route, or choose No inference.' }).click();
  assert.equal(await routeMode.evaluate(node => node === document.activeElement), true);
  await routeMode.selectOption('direct');
  await p.getByRole('combobox', { name: 'Spending' }).selectOption('limited');
  await p.getByRole('spinbutton', { name: 'Budget (USD)' }).fill('0');
  await p.getByRole('button', { name: 'Create team', exact: true }).click();
  assert.equal(await p.getByRole('spinbutton', { name: 'Budget (USD)' }).getAttribute('aria-invalid'), 'true');
  await audit(p, 'team-validation');
  await f.open('#routes?item=coding');
  await p.getByRole('dialog').getByRole('button', { name: 'Edit', exact: true }).click();
  const destination = p.locator('.route-destination').nth(1);
  await destination.locator(':scope > summary').click();
  await destination.getByRole('combobox', { name: 'Model', exact: true }).fill('');
  await destination.locator(':scope > summary').click();
  await p.getByRole('button', { name: 'Save changes', exact: true }).click();
  await p.locator('.error-links a').click();
  assert.equal(await destination.evaluate(node => node.open), true);
  assert.equal(await destination.getByRole('combobox', { name: 'Model', exact: true }).evaluate(node => node === document.activeElement), true);
  await p.getByRole('button', { name: 'Move destination 2 up', exact: true }).click();
  assert.equal(await p.locator('.error-links a').count(), 0, 'Reordering clears links to replaced controls');
  await p.getByRole('button', { name: 'Save changes', exact: true }).click();
  await p.locator('.error-links a').click();
  assert.equal(await p.locator('.route-destination').first().getByRole('combobox', { name: 'Model', exact: true }).evaluate(node => node === document.activeElement), true);
  await f.state('field-errors'); await f.open('#routes?item=coding');
  await p.getByRole('dialog').getByRole('button', { name: 'Edit', exact: true }).click();
  await p.getByRole('textbox', { name: 'Description', exact: true }).fill('Edited description');
  await p.getByRole('button', { name: 'Save changes', exact: true }).click();
  await p.getByRole('link', { name: 'Choose an available model.', exact: true }).click();
  assert.equal(await p.locator('.route-destination').nth(1).getByRole('combobox', { name: 'Model', exact: true }).getAttribute('aria-invalid'), 'true');
});

test('workload filtering and read-only retry retain the selected stopped record', { timeout: 45000 }, async t => {
  const f = await consolePage(t), p = f.page;
  await f.open('#workloads');
  assert.equal(await p.getByRole('button', { name: 'llama3.3:70b', exact: true }).count(), 0);
  await p.getByRole('combobox', { name: 'Show', exact: true }).selectOption('stopped');
  await p.getByRole('button', { name: 'llama3.3:70b', exact: true }).click();
  await p.getByRole('combobox', { name: 'Show', exact: true }).selectOption('active');
  assert.equal(await p.getByRole('button', { name: 'llama3.3:70b', exact: true }).count(), 1);
  await f.state('errors'); await f.open('#workloads');
  await p.getByRole('button', { name: 'Retry reads', exact: true }).waitFor();
  assert.equal(await p.getByText('fixture-request-7be34ac2', { exact: true }).isVisible(), false);
  await f.state('populated'); await p.getByRole('button', { name: 'Retry reads', exact: true }).click();
  await p.getByRole('button', { name: 'qwen3-coder', exact: true }).waitFor();
  assert.equal((await f.requests()).filter(row => row.path === '/runs' && row.method !== 'GET').length, 0);
  await p.getByRole('button', { name: 'qwen3-coder', exact: true }).click();
  await p.getByRole('button', { name: 'Close run details', exact: true }).click();
  assert.equal(await p.evaluate(() => document.activeElement.textContent), 'qwen3-coder');
  await f.open('#routes?item=coding');
  await p.getByRole('button', { name: 'Close route details', exact: true }).click();
  assert.equal(await p.evaluate(() => document.activeElement.textContent), 'coding');
});

test('large topology, mobile list and constrained Chat remain usable', { timeout: 60000 }, async t => {
  const f = await consolePage(t, 'large'), p = f.page;
  await f.open('#overview');
  await audit(p, 'large-topology');
  await p.getByRole('searchbox', { name: 'Find a compute node' }).fill('studio');
  await p.locator('.network-search-results').getByRole('button', { name: 'studio-gpu', exact: true }).click();
  await p.getByRole('button', { name: 'Hide providers', exact: true }).waitFor();
  await p.getByRole('button', { name: 'Hide providers', exact: true }).click();
  await p.getByRole('button', { name: 'Show providers', exact: true }).click();
  await p.getByRole('button', { name: 'List view', exact: true }).click();
  assert.equal(await p.locator('.topology-list-providers [data-selection="local/studio-gpu/vllm"]').count(), 1);
  await p.getByRole('button', { name: 'Refresh', exact: true }).click();
  await p.getByRole('button', { name: 'Map view', exact: true }).waitFor();
  await f.open('#overview', 390, 844);
  assert.equal(await p.getByRole('button', { name: 'Map view', exact: true }).isVisible(), true);
  await f.state('populated');
  for (const [width, height] of [[320, 568], [390, 420], [390, 500], [1280, 500]]) {
    await f.open('#chat', width, height);
    await f.model('qwen3-coder@studio-gpu');
    await audit(p, 'chat-constrained-' + width + '-' + height);
  }
  await f.open('#chat', 390, 420); await f.model('qwen3-coder@studio-gpu');
  await f.state('unverified'); await p.evaluate(() => window.dispatchEvent(new Event('focus')));
  await p.getByRole('button', { name: 'Verify session' }).waitFor({ state: 'attached' });
  await audit(p, 'chat-short-unverified');
  assert.equal(await p.getByRole('button', { name: 'Verify session' }).evaluate(node => { const r = node.getBoundingClientRect(); return document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2) === node; }), true);
});
