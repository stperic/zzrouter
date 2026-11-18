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
  page.on('response', response => {
    const path = new URL(response.url()).pathname;
    if (path.startsWith('/console/') && response.status() >= 400) errors.push('Console asset failed: ' + path + ' (' + response.status() + ')');
  });
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

test('owner setup and account sign-in are focused, accessible and use temporary codes', { timeout: 120000 }, async t => {
  const f = await consolePage(t, 'setup'), { page } = f;
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(f.base);
  await page.getByRole('heading', { name: 'Set up your router', exact: true }).waitFor();
  assert.equal(await page.getByLabel('Username', { exact: true }).count(), 0);
  await audit(page, 'owner-setup-code-phone');
  await page.getByLabel('Temporary code', { exact: true }).fill('synthetic-setup-code');
  await page.getByRole('button', { name: 'Continue', exact: true }).click();
  await page.getByRole('heading', { name: 'Create your owner account', exact: true }).waitFor();
  await page.getByLabel('Name', { exact: true }).fill('Alex Owner');
  await page.getByLabel('Username', { exact: true }).fill('alex');
  await page.getByLabel('Password', { exact: true }).fill('correct long passphrase');
  assert.equal(await page.getByLabel('Password', { exact: true }).getAttribute('autocomplete'), 'new-password');
  await audit(page, 'owner-account-phone');
  await page.getByRole('button', { name: 'Create account', exact: true }).click();
  await page.getByRole('heading', { name: 'Get your first model working', exact: true }).waitFor();
  assert.match(await page.locator('.onboarding-steps li').filter({ hasText: 'Connect a provider' }).innerText(), /Available/);
  assert.match(await page.locator('.onboarding-steps li').filter({ hasText: 'Prepare a model' }).innerText(), /Available/);
  assert.doesNotMatch(await page.locator('.onboarding-steps li').filter({ hasText: 'Test in Chat' }).innerText(), /Done/);
  const writes = (await f.requests()).filter(call => call.method === 'POST');
  assert.equal(writes.find(call => call.path === '/access/setup').payload.token, 'bound-synthetic-setup-code');
  assert.doesNotMatch(page.url(), /synthetic|passphrase/);
  assert.equal(await page.evaluate(() => Object.keys(localStorage).some(key => /token|password|key|grant/.test(key))), false);
  await f.nav('My account');
  await page.getByRole('button', { name: 'Sign out other sessions', exact: true }).waitFor();
  await audit(page, 'owner-my-account-phone');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByLabel('Username', { exact: true }).fill('alex');
  await page.getByLabel('Password', { exact: true }).fill('correct long passphrase');
  assert.equal(await page.getByLabel('Password', { exact: true }).getAttribute('autocomplete'), 'current-password');
  await audit(page, 'account-sign-in-phone');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).waitFor();
  assert.equal((await f.requests()).filter(call => call.path === '/session/password').length, 1);
});

test('People details, editing and invitations share one panel and do not retain link secrets', { timeout: 120000 }, async t => {
  const f = await consolePage(t, 'person'), { page } = f;
  await f.open('#people', 1280, 800);
  await page.getByRole('button', { name: 'Morgan Developer', exact: true }).click();
  assert.doesNotMatch(await page.getByRole('dialog').innerText(), /\bnull\b/, 'non-owner details have no empty badge text');
  await page.getByRole('button', { name: 'Edit', exact: true }).click();
  assert.equal(await page.getByRole('dialog').count(), 1);
  await page.getByLabel('Name', { exact: true }).fill('Morgan Updated');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('heading', { name: 'Morgan Updated', exact: true }).waitFor();
  await page.getByRole('button', { name: 'Close person details', exact: true }).click();
  await page.getByRole('button', { name: '+ Invite person', exact: true }).click();
  await page.getByText('Inherited team access', { exact: true }).waitFor();
  await audit(page, 'invite-person-desktop');
  await page.getByRole('button', { name: 'Create invitation', exact: true }).click();
  const link = await page.getByLabel('Invitation link', { exact: true }).inputValue();
  assert.match(link, /#invite\?token=/);
  await page.setViewportSize({ width: 390, height: 844 });
  await audit(page, 'invite-link-phone');
  await page.goto(link);
  await page.getByRole('heading', { name: 'You’re invited', exact: true }).waitFor();
  assert.doesNotMatch(await page.locator('body').innerText(), /\bnull\b/, 'anonymous invitation review has no account placeholder text');
  assert.doesNotMatch(page.url(), /token=|synthetic/);
  const before = (await f.requests()).filter(call => call.path === '/access/console-grants/exchange').length;
  assert.equal(before, 0, 'opening an invitation does not consume it before review');
  await audit(page, 'invite-review-phone');
  await page.getByRole('button', { name: 'Continue', exact: true }).click();
  await page.getByLabel('Name', { exact: true }).fill('Taylor');
  await page.getByLabel('Username', { exact: true }).fill('taylor');
  await page.getByLabel('Password', { exact: true }).fill('another long passphrase');
  await page.getByRole('button', { name: 'Create account', exact: true }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).waitFor();
  assert.equal((await f.requests()).filter(call => call.path === '/access/invitations/accept').length, 1);
  assert.equal(await page.evaluate(() => JSON.stringify(localStorage).includes('synthetic-invitation')), false);
});

test('recovery and password changes use the same account session flow', { timeout: 120000 }, async t => {
  const f = await consolePage(t, 'person'), { page } = f;
  await f.open('#people?item=member');
  await page.getByRole('button', { name: 'Create recovery link', exact: true }).click();
  const link = await page.getByLabel('Invitation link', { exact: true }).inputValue();
  await page.goto(link);
  await page.getByRole('heading', { name: 'Reset your password', exact: true }).waitFor();
  assert.doesNotMatch(page.url(), /token=/);
  await page.getByRole('button', { name: 'Continue', exact: true }).click();
  await page.getByLabel('New password', { exact: true }).fill('replacement passphrase');
  await audit(page, 'password-recovery');
  await page.getByRole('button', { name: 'Save password and sign in', exact: true }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).waitFor();
  await f.nav('My account');
  await page.getByText('Change password', { exact: true }).first().click();
  await page.getByLabel('Current password', { exact: true }).fill('replacement passphrase');
  await page.getByLabel('New password', { exact: true }).fill('another replacement passphrase');
  await page.getByRole('button', { name: 'Change password', exact: true }).click();
  await page.getByText('Password changed. Other sessions have been signed out.', { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Sign out other sessions', exact: true }).click();
  await page.getByText('Other sessions and authorizations were revoked.', { exact: true }).waitFor();
  const paths = (await f.requests()).filter(call => call.method === 'POST').map(call => call.path);
  for (const path of ['/access/recovery', '/access/password', '/access/sessions/revoke']) assert.equal(paths.filter(value => value === path).length, 1);
});

test('computer pairing previews the transfer and requires a new review after an edit', { timeout: 90000 }, async t => {
  const f = await consolePage(t), { page } = f;
  await f.open('#nodes?item=add', 390, 844);
  await page.getByRole('button', { name: 'I have pairing details', exact: true }).click();
  const transfer = { type: 'zzrouter-pairing', version: 1, node_name: 'new-worker', code: 'synthetic-pair-code', worker_url: 'https://worker.example:9091', worker_fingerprint: 'sha256:synthetic-public-fingerprint', expires_at: '2030-01-01T00:00:00Z' };
  const field = page.getByLabel('Pairing details', { exact: true });
  await field.fill(JSON.stringify(transfer));
  assert.equal(await page.getByRole('button', { name: 'Approve computer', exact: true }).isDisabled(), true);
  await page.getByRole('button', { name: 'Review computer', exact: true }).click();
  await page.getByText(transfer.worker_fingerprint, { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Change pairing details', exact: true }).click();
  await field.fill(JSON.stringify({ ...transfer, node_name: 'edited-worker' }));
  assert.equal(await page.getByRole('button', { name: 'Approve computer', exact: true }).isDisabled(), true);
  await field.fill(JSON.stringify(transfer));
  await page.getByRole('button', { name: 'Review computer', exact: true }).click();
  await audit(page, 'pairing-review-phone');
  await page.getByRole('button', { name: 'Approve computer', exact: true }).click();
  await page.getByRole('button', { name: 'View node', exact: true }).waitFor();
  const write = (await f.requests()).find(call => call.path === '/cluster/pairing/accept');
  assert.deepEqual(write.payload, { transfer });
  assert.equal(await field.inputValue(), '');
});

test('color themes preserve work, persist across tabs and cover light and dark surfaces', { timeout: 180000 }, async t => {
  const f = await consolePage(t), { page } = f;
  await f.open('#overview');
  // Clearing the layout status resizes the canvas. Let its ResizeObserver finish
  // before measuring the camera, so this asserts the theme change alone.
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator('html').getAttribute('data-theme'), 'claude-web');
  const graph = () => page.evaluate(() => {
    const cy = document.querySelector('.topology-canvas')._cyreg.cy;
    return { pan: cy.pan(), zoom: cy.zoom(), positions: cy.nodes().map(node => node.position()), color: cy.nodes().first().style('color') };
  });
  const before = await graph();
  await page.getByLabel('Color theme', { exact: true }).selectOption('claude-web-dark');
  const after = await graph();
  assert.deepEqual(after.pan, before.pan); assert.equal(after.zoom, before.zoom); assert.deepEqual(after.positions, before.positions);
  assert.notEqual(after.color, before.color);
  const other = await page.context().newPage();
  await other.goto(f.base + '#chat');
  await other.getByRole('textbox', { name: 'Message', exact: true }).waitFor();
  assert.equal(await other.locator('html').getAttribute('data-theme'), 'claude-web-dark');
  for (const theme of ['claude-web', 'claude-web-dark', 'claude-code-light', 'claude-code-dark']) {
    await page.getByLabel('Color theme', { exact: true }).selectOption(theme);
    await other.waitForFunction(theme => document.documentElement.dataset.theme === theme, theme);
    await audit(page, theme + '-overview');
    await f.open('#routes?item=coding');
    await page.getByRole('button', { name: 'Edit', exact: true }).click();
    await audit(page, theme + '-editor');
    await f.open('#chat', 390, 844);
    await f.model('coding');
    const message = page.getByRole('textbox', { name: 'Message', exact: true });
    await message.fill('Keep this draft through a theme change.');
    await page.getByRole('button', { name: 'Navigation', exact: true }).click();
    await page.getByLabel('Color theme', { exact: true }).selectOption(theme === 'claude-web' ? 'claude-code-dark' : 'claude-web');
    await page.getByLabel('Color theme', { exact: true }).selectOption(theme);
    await audit(page, theme + '-mobile-navigation');
    await page.getByRole('button', { name: 'Navigation', exact: true }).click();
    assert.equal(await message.inputValue(), 'Keep this draft through a theme change.');
    await audit(page, theme + '-chat-phone');
    await f.open('#overview');
  }
  await page.reload();
  await page.locator('.topology-status:empty').waitFor({ state: 'attached' });
  assert.equal(await page.getByLabel('Color theme', { exact: true }).inputValue(), 'claude-code-dark');
  await page.evaluate(() => localStorage.setItem('zzrouter.console.theme', 'unknown-preset'));
  await page.reload();
  await page.locator('#content h1').waitFor();
  assert.equal(await page.getByLabel('Color theme', { exact: true }).inputValue(), 'claude-web');
  await page.addInitScript(() => Object.defineProperty(window, 'localStorage', { get() { throw new DOMException('Blocked', 'SecurityError'); } }));
  await page.reload();
  await page.locator('#content h1').waitFor();
  await page.getByLabel('Color theme', { exact: true }).selectOption('claude-code-dark');
  await page.getByText('Applied for this tab. Browser storage is unavailable.').waitFor();
  assert.equal(await page.locator('html').getAttribute('data-theme'), 'claude-code-dark');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('button', { name: 'Sign in', exact: true }).waitFor();
  assert.equal(await page.locator('html').getAttribute('data-theme'), 'claude-code-dark');
  assert.equal(await page.getByLabel('Color theme', { exact: true }).count(), 0, 'appearance stays out of the first sign-in task');
  await audit(page, 'theme-sign-in-dark');
});

test('all console views and shared editors fit desktop and phone viewports', { timeout: 180000 }, async t => {
  const f = await consolePage(t);
  const scenes = [
    ['overview', '#overview'], ['nodes', '#nodes'], ['node-editor', '#nodes?item=node%2Fstudio-gpu', 'Edit'],
    ['add-node', '#nodes?item=add'], ['providers', '#providers'], ['cloud-providers', '#providers?item=cloud'],
    ['registries', '#providers?item=registries'], ['local-provider', '#providers?item=local%2Follama'],
    ['cloud-editor', '#providers?item=cloud%2Fgroq', 'Edit'], ['routes', '#routes'], ['route-editor', '#routes?item=coding', 'Edit'],
    ['create-route', '#routes?mode=create'], ['clients', '#clients'], ['keys', '#access'], ['key-editor', '#access?item=keys%2Feditor', 'Edit'],
    ['team-editor', '#access?item=teams%2Fengineering', 'Edit'], ['create-team', '#access?item=teams&mode=create'],
    ['people', '#people'], ['workloads', '#workloads'], ['jobs', '#workloads?mode=jobs'], ['system', '#settings'], ['chat', '#chat'],
  ];
  for (const width of [1280, 390]) for (const [name, hash, action] of scenes) {
    await f.open(hash, width, width === 390 ? 844 : 800);
    if (action) await f.page.getByRole('dialog').getByRole('button', { name: action, exact: true }).click();
    await audit(f.page, name + '-' + width);
  }
  await f.state('user'); await f.open('#connections', 390, 844); await audit(f.page, 'my-access-user');
  assert.doesNotMatch(await f.page.locator('#content').innerText(), /\bnull\b/, 'app-key access has no account placeholder text');
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

test('Discover shows each model once and keeps IDs for details and ambiguous names', async t => {
  const f = await consolePage(t), { page } = f;
  const model = { provider: 'anthropic', display_name: 'Claude Fable 5.2', api_id: 'claude-fable-5-2', is_cloud: true, context_length: 1000000, created_at: '2026-11-09T18:30:00Z' };
  const first = { ...model, display_name: 'Claude preview', api_id: 'claude-preview-first', modified_at: '2026-11-10T00:10:00Z' };
  const second = { ...first, api_id: 'claude-preview-second', created_at: undefined, modified_at: '' };
  await page.route('**/search?**', route => {
    const type = new URL(route.request().url()).searchParams.get('type');
    const results = type === 'providers' ? [{ name: 'anthropic', display_name: 'Anthropic', is_cloud: true, configured: true }] :
      [model, first, second, model].map(discovery => ({ discovery }));
    return route.fulfill({ json: { data: { results } } });
  });
  await page.route('**/models/card/anthropic/**', route => route.fulfill({ json: { data: { discovery: model } } }));
  await f.open('#models?mode=discover');
  const cards = page.locator('.model-list-results button');
  await cards.first().waitFor();
  assert.equal(await cards.count(), 3);
  const fable = cards.filter({ hasText: model.display_name });
  assert.equal(await fable.count(), 1);
  assert.doesNotMatch(await fable.innerText(), /claude-fable-5-2/);
  assert.equal(await fable.locator('time').getAttribute('datetime'), model.created_at);
  assert.match(await fable.innerText(), /Published Nov 9, 2026/);
  assert.equal(await cards.filter({ hasText: first.api_id }).count(), 1);
  assert.equal(await cards.filter({ hasText: second.api_id }).count(), 1);
  assert.equal(await cards.filter({ hasText: first.api_id }).locator('time').getAttribute('datetime'), first.modified_at);
  assert.match(await cards.filter({ hasText: first.api_id }).innerText(), /Updated Nov 10, 2026/);
  assert.equal(await cards.filter({ hasText: second.api_id }).locator('time').count(), 0);
  await fable.click();
  await page.getByRole('dialog').getByText(model.api_id, { exact: true }).waitFor();
});

test('Discover keeps capabilities, libraries and weight formats on cards and puts other tags in details', async t => {
  const f = await consolePage(t), { page } = f;
  const vision = { discovery: { provider: 'huggingface', api_id: 'example/vision-model', tags: [
    'transformers', 'safetensors', 'gguf', 'model-family', 'image-text-to-text', 'image-to-text', 'conversational', 'license:apache-2.0',
  ] }, pipeline_tag: 'image-text-to-text', library_name: 'transformers' };
  const video = { discovery: { provider: 'huggingface', api_id: 'example/video-model',
    tags: ['diffusers', 'diffusion-single-file', 'image-to-video', 'video-to-video', 'audio-to-video'] }, pipeline_tag: 'text-to-video' };
  await page.route('**/search?**', route => {
    const type = new URL(route.request().url()).searchParams.get('type');
    return route.fulfill({ json: { data: { results: type === 'providers' ? [{ name: 'huggingface' }] : [vision, video] } } });
  });
  await page.route('**/models/card/huggingface/**', route => route.fulfill({ json: { data: vision } }));
  await f.open('#models?mode=discover');
  const cards = page.locator('.model-list-results button');
  await cards.first().waitFor();
  const visionCard = cards.filter({ hasText: vision.discovery.api_id });
  const videoCard = cards.filter({ hasText: video.discovery.api_id });
  assert.deepEqual(await visionCard.locator('.model-tags .badge').allTextContents(), ['Vision', 'Transformers', 'Safetensors', 'GGUF']);
  assert.deepEqual(await videoCard.locator('.model-tags .badge').allTextContents(), ['Video generation', 'Diffusers']);
  assert.equal(await visionCard.getByText('8 tags', { exact: true }).isVisible(), true);
  assert.doesNotMatch(await visionCard.innerText(), /model-family|license:|\+\d+ more/);
  await audit(page, 'discover-primary-tags-desktop');
  await visionCard.click();
  const details = page.getByRole('dialog');
  const allTags = details.locator('details.discovery-all-tags');
  await allTags.getByText('All tags', { exact: true }).click();
  assert.deepEqual(await allTags.locator('.badge').allTextContents(), vision.discovery.tags);
  await page.setViewportSize({ width: 390, height: 844 });
  await audit(page, 'discover-all-tags-phone');
});

test('Discover renders provider links and metadata across catalogs', { timeout: 60000 }, async t => {
  const f = await consolePage(t), { page } = f;
  const sources = ['ollama', 'huggingface', 'openai', 'anthropic', 'openrouter', 'azure', 'cloudflare', 'gemini', 'groq', 'ollama-cloud'];
  const cards = Object.fromEntries(sources.map(provider => {
    const url = provider === 'huggingface' ? 'https://huggingface.co/org/model' : 'https://catalog.example/' + provider + '/model';
    const discovery = {
      provider, url, api_id: 'org/model', display_name: provider + ' model',
      is_cloud: !['ollama', 'huggingface'].includes(provider),
      description: 'Try [Jev](https://openrouter.ai/typesafe/jev-router).',
      modified_at: '2026-11-10T00:10:00Z',
    };
    return [provider, { discovery }];
  }));
  const hf = cards.huggingface;
  hf.discovery.readme = '# Guide\n\n[Install](./install.md) · [Overview](#guide)\n\n' +
    '| Setting | Value |\n| --- | --- |\n| Precision | BF16 |';
  hf.siblings = [{ rfilename: 'weights/model file.safetensors', size: 1024 }];
  hf.safetensors = { total: 7000000000 };
  const router = cards.openrouter;
  router.discovery.description += ' See https://example.org/bare. <a href="/docs">HTML link</a>\n\n' +
    '[Unsupported destination](javascript:alert%281%29) ' +
    '[![Badge](https://example.org/badge.svg)](https://example.org/badge)\n\n' +
    '<script>untrusted script</script>\n\nLast paragraph.';
  Object.assign(router.discovery, { price_prompt: -1, price_complete: -1 });
  Object.assign(router, {
    architecture: { input_modalities: ['text', 'image'], output_modalities: ['text'] },
    top_provider: { max_completion_tokens: 16384 },
    links: { details: '/api/v1/endpoints' },
    supported_parameters: ['temperature', 'top_p'],
    absent: null,
  });
  const searched = [];
  await page.route('**/search?**', route => {
    const query = new URL(route.request().url()).searchParams;
    if (query.get('type') === 'providers') return route.fulfill({ json: { data: { results: [
      ...sources.map(name => ({ name, is_cloud: cards[name].discovery.is_cloud, sort_options: ['newest'], discovery_available: true })),
      { name: 'bedrock', is_cloud: true, discovery_available: false, discovery_supported: false },
    ] } } });
    const provider = query.get('provider');
    searched.push(provider);
    return route.fulfill({ json: { data: { results: cards[provider] ? [cards[provider]] : [] } } });
  });
  await page.route('**/models/card/**', route => {
    const provider = new URL(route.request().url()).pathname.split('/card/')[1].split('/')[0];
    return route.fulfill({ json: { data: cards[provider] } });
  });
  await f.open('#models?mode=discover');
  const source = page.getByLabel('Registry or cloud provider');
  const drawer = page.getByRole('dialog');
  const link = name => drawer.getByRole('link', { name, exact: true });
  for (const provider of sources) {
    await source.selectOption(provider);
    const card = page.locator('.model-list-results button').filter({ hasText: provider + ' model' });
    await card.waitFor();
    assert.doesNotMatch(await card.innerText(), /\[Jev\]|\(https:/);
    assert.equal(await card.locator('a').count(), 0);
    await card.click();
    assert.equal(await link('Jev').getAttribute('href'), 'https://openrouter.ai/typesafe/jev-router');
    assert.equal(await link('View model on provider').getAttribute('href'), cards[provider].discovery.url);
    assert.equal(await drawer.getByText('null', { exact: true }).count(), 0);
    assert.equal(await drawer.getByText('org/model', { exact: true }).isVisible(), true);
    assert.equal(await drawer.locator('time').getAttribute('datetime'), '2026-11-10T00:10:00Z');
    if (provider === 'openrouter') {
      assert.equal(await link('https://example.org/bare').getAttribute('href'), 'https://example.org/bare');
      assert.equal(await link('HTML link').getAttribute('href'), 'https://catalog.example/docs');
      assert.equal(await link('Badge').getAttribute('href'), 'https://example.org/badge');
      assert.equal(await link('Endpoint details').getAttribute('href'), 'https://catalog.example/api/v1/endpoints');
      assert.equal(await link('Unsupported destination').count(), 0);
      assert.equal(await drawer.locator('a a, script').count(), 0);
      assert.doesNotMatch(await drawer.innerText(), /untrusted script/);
      assert.equal(await drawer.getByText('Last paragraph.', { exact: true }).isVisible(), true);
      assert.equal(await drawer.getByText('text, image', { exact: true }).isVisible(), true);
      assert.equal(await drawer.getByText('16,384', { exact: true }).isVisible(), true);
      assert.equal(await drawer.getByText('Varies by routed model', { exact: true }).count(), 2);
      await drawer.getByText('Provider details', { exact: true }).click();
      await drawer.getByText('temperature, top_p', { exact: true }).waitFor();
      await audit(page, 'discover-provider-details-desktop');
    }
    if (provider === 'huggingface') {
      await drawer.getByText('Model card', { exact: true }).click();
      assert.equal(await link('Install').getAttribute('href'), hf.discovery.url + '/blob/main/install.md');
      assert.equal(await link('Overview').getAttribute('href'), hf.discovery.url + '#guide');
      assert.equal(await drawer.getByRole('cell', { name: 'BF16', exact: true }).isVisible(), true);
      await drawer.getByText('Repository files (1)', { exact: true }).click();
      assert.equal(await link('weights/model file.safetensors').getAttribute('href'), hf.discovery.url + '/blob/main/weights/model%20file.safetensors');
      await page.setViewportSize({ width: 390, height: 844 });
      await audit(page, 'discover-model-card-phone');
      await page.setViewportSize({ width: 1440, height: 900 });
    }
    await drawer.getByRole('button', { name: 'Close discover model' }).click();
  }
  await source.selectOption('bedrock');
  assert.equal(await source.locator('optgroup[label="Catalog unavailable"] option').count(), 1);
  await page.getByText('Model discovery is unavailable through this connection.', { exact: true }).waitFor();
  assert.equal(await page.getByRole('button', { name: 'Search', exact: true }).isDisabled(), true);
  assert.equal(searched.includes('bedrock'), false);
});

test('Discover expands OpenRouter excerpts on demand and keeps a provider link if unavailable', async t => {
  const f = await consolePage(t), { page } = f;
  const excerpt = 'This model supports text and images. Thinking can...';
  const full = 'This model supports text and images. Thinking can be turned off per request. See the [model guide](https://example.com/guide).';
  const read = [];
  const models = ['example/supported', 'example/unavailable'].map(api_id => ({
    discovery: { provider: 'openrouter', api_id, is_cloud: true, url: 'https://openrouter.ai/' + api_id, description: excerpt },
  }));
  await page.route('**/search?**', route => {
    const providers = new URL(route.request().url()).searchParams.get('type') === 'providers';
    return route.fulfill({ json: { data: { results: providers ? [{ name: 'openrouter', is_cloud: true }] : models } } });
  });
  await page.route('**/models/card/openrouter/**', route => {
    const url = new URL(route.request().url());
    const model = models.find(item => url.pathname.endsWith('/' + item.discovery.api_id));
    if (url.searchParams.get('full_description') !== 'true') return route.fulfill({ json: { data: model } });
    read.push(model.discovery.api_id);
    const description = model === models[0] ? full : excerpt;
    return route.fulfill({ json: { data: { discovery: { ...model.discovery, description } } } });
  });
  await f.open('#models?mode=discover');
  const drawer = page.getByRole('dialog');
  const expand = drawer.getByRole('button', { name: 'Read full description' });
  await page.getByRole('button', { name: 'example/supported', exact: true }).click();
  await expand.waitFor();
  assert.deepEqual(read, []);
  assert.equal(await drawer.getByText(excerpt, { exact: true }).isVisible(), true);
  await expand.click();
  const collapse = drawer.getByRole('button', { name: 'Show less' });
  assert.equal(await collapse.getAttribute('aria-expanded'), 'true');
  assert.equal(await drawer.getByRole('link', { name: 'model guide' }).getAttribute('href'), 'https://example.com/guide');
  await collapse.click();
  assert.equal(await drawer.getByText(excerpt, { exact: true }).isVisible(), true);
  await expand.click();
  await collapse.waitFor();
  assert.deepEqual(read, ['example/supported']);
  await drawer.getByRole('button', { name: 'Close discover model' }).click();
  await page.getByRole('button', { name: 'example/unavailable', exact: true }).click();
  await expand.click();
  assert.equal(await drawer.getByRole('link', { name: 'Read on OpenRouter' }).getAttribute('href'), models[1].discovery.url);
  assert.equal(await drawer.getByText(excerpt, { exact: true }).isVisible(), true);
  assert.deepEqual(read, ['example/supported', 'example/unavailable']);
});

test('Discover sends a cloud model search and sort to the coordinator before pagination', async t => {
  const f = await consolePage(t), { page } = f;
  const queries = [];
  await page.route('**/search?**', route => {
    const query = new URL(route.request().url()).searchParams;
    if (query.get('type') === 'providers') return route.fulfill({ json: { data: { results: [
      { name: 'openai', is_cloud: true, configured: true, sort_options: ['name', 'newest'],
        client_filter: true, client_sorts: ['name', 'newest'] },
    ] } } });
    queries.push(Object.fromEntries(query));
    const id = query.get('q') === 'mini' ? 'model-mini' : 'another-model';
    return route.fulfill({ json: { data: { results: [{ discovery: { provider: 'openai', api_id: id, is_cloud: true } }] } } });
  });
  await f.open('#models?mode=discover');
  await page.getByRole('button', { name: 'another-model', exact: true }).waitFor();
  await page.getByRole('searchbox', { name: 'Search', exact: true }).fill('mini');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await page.getByRole('button', { name: 'model-mini', exact: true }).waitFor();
  await page.getByLabel('Sort', { exact: true }).selectOption({ label: 'Name A to Z' });
  await page.getByRole('button', { name: 'model-mini', exact: true }).waitFor();
  assert.deepEqual([queries.at(-1).q, queries.at(-1).sort, queries.at(-1).order], ['mini', 'name', 'asc']);
  assert.doesNotMatch(await page.locator('.discovery-filters').innerText(), /current provider page/);
});

test('Discover opens provider setup before searching an unconfigured cloud catalog', async t => {
  const f = await consolePage(t), { page } = f;
  const searched = [], cards = [];
  let openRouterReady = false;
  await page.route('**/models/card/openrouter/**', route => {
    cards.push(new URL(route.request().url()).pathname);
    return route.fulfill({ json: { code: 'provider_connection_unavailable' }, status: 503 });
  });
  await page.route('**/search?**', route => {
    const query = new URL(route.request().url()).searchParams;
    if (query.get('type') === 'providers') return route.fulfill({ json: { data: { results: [
      { name: 'openrouter', display_name: 'OpenRouter', is_cloud: true, configured: true, enabled: openRouterReady, discovery_available: openRouterReady },
      { name: 'groq', display_name: 'Groq', is_cloud: true, configured: true, enabled: false, discovery_available: true },
    ] } } });
    const provider = query.get('provider');
    searched.push(provider);
    return route.fulfill({ json: { data: { results: [
      { discovery: { provider, api_id: 'available-model', is_cloud: true } },
    ] } } });
  });
  await f.open('#models?mode=discover');
  const row = page.getByRole('button', { name: 'available-model', exact: true });
  const source = page.getByLabel('Registry or cloud provider');
  const search = page.getByRole('button', { name: 'Search', exact: true });
  await row.waitFor();
  assert.deepEqual(await source.locator('optgroup').evaluateAll(groups => groups.map(group => group.label)), ['Available', 'Setup required']);
  assert.deepEqual(await source.locator('optgroup[label="Available"] option').allTextContents(), ['Groq']);
  assert.deepEqual(await source.locator('optgroup[label="Setup required"] option').allTextContents(), ['OpenRouter']);
  assert.equal(await source.inputValue(), 'groq');
  await source.selectOption('openrouter');
  assert.equal(await page.getByText('Add the provider’s credentials, such as an API key, to browse its models.', { exact: true }).isVisible(), true);
  const configure = page.getByRole('button', { name: 'Configure provider', exact: true });
  await configure.waitFor();
  assert.equal(await search.isDisabled(), true);
  assert.equal(await page.getByLabel('Sort', { exact: true }).isDisabled(), true);
  assert.equal(await page.getByRole('searchbox', { name: 'Search', exact: true }).isDisabled(), true);
  assert.equal(await row.count(), 0);
  await audit(page, 'discover-provider-setup');
  await source.selectOption('groq');
  await row.waitFor();
  assert.equal(await search.isEnabled(), true);
  await source.selectOption('openrouter');
  await configure.click();
  await page.locator('[data-provider="openrouter"][aria-current="true"]').waitFor();
  assert.deepEqual(searched, ['groq', 'groq']);
  await f.open('#models?item=openrouter%2Fexample%2Fold-model&mode=discover');
  await configure.waitFor();
  assert.equal(await page.getByRole('dialog').count(), 0);
  assert.equal(await source.inputValue(), 'openrouter');
  assert.equal(await search.isDisabled(), true);
  assert.deepEqual(cards, []);
  assert.deepEqual(searched, ['groq', 'groq']);
  openRouterReady = true;
  await page.getByRole('button', { name: 'Refresh providers', exact: true }).click();
  await row.waitFor();
  assert.equal(await source.inputValue(), 'openrouter');
  assert.equal(await search.isEnabled(), true);
  assert.deepEqual(await source.locator('optgroup[label="Available"] option').allTextContents(), ['OpenRouter', 'Groq']);
  assert.deepEqual(searched, ['groq', 'groq', 'openrouter']);
  assert.deepEqual(cards, []);
});

test('Available and Discover share desktop filters, compact rows, and mobile layout', async t => {
  const f = await consolePage(t), { page } = f;
  await f.open('#models', 1280, 800);
  const rows = page.locator('.model-list-results > button');
  const toolbar = page.locator('.model-toolbar');
  const bottomEdges = () => toolbar.locator('select, input, button').evaluateAll(controls => controls.map(control => control.getBoundingClientRect().bottom));
  const rowPadding = () => rows.first().evaluate(row => getComputedStyle(row).padding);
  const installed = rows.getByText('qwen3-coder', { exact: true });
  await installed.waitFor();
  const availablePadding = await rowPadding();
  assert.equal(new Set(await bottomEdges()).size, 1, 'Available controls align on one desktop row');
  await page.getByRole('combobox', { name: 'Provider', exact: true }).selectOption('vllm');
  assert.equal(await rows.count(), 1);
  assert.match(await rows.first().locator('.model-row-stats').innerText(), /vllm.*studio-gpu/s);
  await page.getByRole('searchbox', { name: 'Search', exact: true }).fill('no such model');
  await page.getByText('No models match these filters.', { exact: true }).waitFor();
  await page.getByRole('searchbox', { name: 'Search', exact: true }).fill('');
  await installed.waitFor();
  await page.setViewportSize({ width: 390, height: 844 });
  await audit(page, 'available-model-list-phone');
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.getByRole('button', { name: 'Discover', exact: true }).click();
  await rows.first().waitFor();
  assert.equal(new Set(await bottomEdges()).size, 1, 'Discover controls align on one desktop row');
  assert.equal(await rowPadding(), availablePadding);
  assert.deepEqual(await toolbar.locator('label > span').allTextContents(), ['Registry or cloud provider', 'Search', 'Sort']);
  await audit(page, 'discover-model-list-desktop');
});

test('Ollama Discover searches the catalog, sorts before pagination and reviews eligible downloads', { timeout: 60000 }, async t => {
  const f = await consolePage(t), { page } = f;
  const model = { provider: 'ollama', api_id: 'qwen3.8', display_name: 'qwen3.8', downloads: 63100, size_bytes: 4800000000, modified_at: '2 days ago',
    tags: ['tools', 'vision', 'thinking', 'Tools'], description: 'A general-purpose model for coding and image tasks.', variant_count: 13, parameter_sizes: ['27b'] };
  const older = { ...model, api_id: 'qwen2.5', display_name: 'qwen2.5', modified_at: '2 years ago' };
  const queries = [];
  let eligibility = false;
  await page.route('**/search?**', route => {
    const query = Object.fromEntries(new URL(route.request().url()).searchParams);
    if (query.type === 'providers') return route.fulfill({ json: { data: { results: [{
      name: 'ollama', display_name: 'Ollama', client_filter: true, client_sorts: ['popular', 'newest', 'name'], has_tags: true,
    }] } } });
    queries.push(query);
    const discovery = (query.sort === 'modified') === !query.offset ? model : older;
    return route.fulfill({ json: { data: { results: [{ discovery }], total: 2, is_total_exact: true, pagination: { offset: Number(query.offset || 0), limit: 1, has_next: !query.offset } } } });
  });
  await page.route('**/models/card/ollama/**', route => route.fulfill({ json: { data: { discovery: model } } }));
  await page.route('**/registries/ollama/tags?**', route => route.fulfill({ json: { data: { tags: [
    { name: '27b', size: '18GB', context: '256K', input: 'Text, Image' },
    { name: '27b-mxfp8', size: '19GB', context: '256K', input: 'Text', is_mlx: true },
    { name: '70b-cloud', size: '-', is_cloud: true },
  ] } } }));
  await page.route(/\/zzrouter\/v1\/nodes(?:\?|$)/, route => route.fulfill({ json: { data: [
    { name: 'studio-gpu', health_status: 'healthy', providers: [{ key: 'ollama', running: true }] },
    { name: 'office-pc', health_status: 'healthy', providers: [{ key: 'ollama', running: true }] },
    { name: 'offline', health_status: 'down', providers: [{ key: 'ollama', running: true }] },
    { name: 'ollama-stopped', health_status: 'healthy', providers: [{ key: 'ollama', running: false }] },
    { name: 'small-node', health_status: 'healthy', providers: [{ key: 'ollama', running: true }] },
  ] } }));
  await page.route('**/nodes/compatible?**', route => {
    const query = new URL(route.request().url()).searchParams;
    const mlx = query.get('model') === 'qwen3.8:27b-mxfp8';
    assert.deepEqual(Object.fromEntries(query), { model: mlx ? 'qwen3.8:27b-mxfp8' : 'qwen3.8:27b', registry: 'ollama', provider: 'ollama', ...(mlx ? { format: 'mlx' } : {}) });
    return route.fulfill({ json: { data: [], metadata: { assessments: [
      { node: 'studio-gpu', provider: 'ollama', status: 'unknown', unknown_cause: mlx ? 'provider_not_eligible' : eligibility ? 'budget_not_sized' : 'node_unreachable' },
      { node: 'office-pc', provider: 'ollama', status: 'unknown', unknown_cause: 'provider_not_eligible' },
      { node: 'office-pc', provider: 'strata', status: 'fit' },
      { node: 'offline', provider: 'ollama', status: 'fit' },
      { node: 'ollama-stopped', provider: 'ollama', status: 'fit' },
      { node: 'small-node', provider: 'ollama', status: 'no_fit' },
    ] } } });
  });
  await f.open('#models?mode=discover', 1380, 879);
  const cards = page.locator('.model-list-results button');
  await cards.first().waitFor();
  assert.equal(await page.getByText('More filters', { exact: true }).isVisible(), false);
  assert.doesNotMatch(await page.locator('.discovery-filters').innerText(), /this page|current provider page/);
  await page.getByRole('searchbox', { name: 'Search', exact: true }).fill('qwen');
  await page.getByLabel('Sort', { exact: true }).selectOption({ label: 'Recently updated' });
  await cards.getByText('qwen3.8', { exact: true }).waitFor();
  assert.deepEqual([queries.at(-1).q, queries.at(-1).sort, queries.at(-1).order], ['qwen', 'modified', 'desc']);
  assert.equal(await cards.first().getByText('Updated 2 days ago', { exact: true }).count(), 1);
  assert.doesNotMatch(await cards.first().innerText(), /GiB/);
  assert.equal(await cards.first().locator('.model-row-description').innerText(), model.description);
  const stats = cards.first().locator('.model-row-stats');
  assert.equal(await stats.getByText('63.1K', { exact: true }).isVisible(), true);
  assert.equal(await stats.getByText('13 tags', { exact: true }).isVisible(), true);
  assert.equal(await stats.getByTitle('63,100 downloads').count(), 1);
  assert.equal(await stats.locator('[aria-hidden="true"].discovery-stat-icon').count(), 2);
  assert.equal(await stats.locator('.badge').innerText(), '27b');
  const pills = cards.first().locator('.model-tags .badge');
  assert.deepEqual(await pills.allTextContents(), ['Tools', 'Vision', 'Thinking']);
  const colors = await pills.evaluateAll(items => items.map(item => getComputedStyle(item).backgroundColor));
  assert.equal(new Set(colors).size, 3);
  await page.getByRole('button', { name: 'Next', exact: true }).click();
  await cards.getByText('qwen2.5', { exact: true }).waitFor();
  assert.deepEqual([queries.at(-1).q, queries.at(-1).sort, queries.at(-1).offset], ['qwen', 'modified', '1']);
  await page.getByLabel('Sort', { exact: true }).selectOption({ label: 'Name A to Z' });
  await page.getByRole('button', { name: 'Next', exact: true }).waitFor();
  assert.deepEqual([queries.at(-1).sort, queries.at(-1).order, queries.at(-1).offset], ['name', 'asc', undefined]);
  await page.getByLabel('Sort', { exact: true }).selectOption('modified');
  await page.getByRole('button', { name: /^qwen3\.8/ }).click();
  const dialog = page.getByRole('dialog');
  const detailPills = dialog.locator('.detail-body > .model-tags .badge');
  await detailPills.first().waitFor();
  assert.deepEqual(await detailPills.allTextContents(), ['Tools', 'Vision', 'Thinking']);
  await dialog.getByRole('button', { name: 'Download', exact: true }).click();
  const provider = dialog.getByLabel('Local provider', { exact: true });
  const review = dialog.getByRole('button', { name: 'Review download', exact: true });
  const version = dialog.getByLabel('Version tag', { exact: true });
  await dialog.getByText('Choose a version to see eligible nodes.', { exact: true }).waitFor();
  assert.equal(await dialog.locator('input[name^="node_"]').count(), 0);
  await version.selectOption('27b-mxfp8');
  await dialog.getByText('No running Ollama provider on a healthy node was confirmed eligible for this version.', { exact: true }).waitFor();
  assert.equal(await dialog.getByText('MLX · Text · 256K context', { exact: true }).count(), 1);
  assert.equal(await review.isEnabled(), false);
  await version.selectOption('27b');
  await dialog.getByRole('button', { name: 'Retry eligibility' }).waitFor();
  eligibility = true;
  await dialog.getByRole('button', { name: 'Retry eligibility' }).click();
  await dialog.getByRole('checkbox', { name: 'studio-gpu', exact: true }).waitFor();
  assert.equal(await provider.inputValue(), 'ollama');
  assert.deepEqual(await provider.locator('option').allTextContents(), ['Ollama']);
  assert.deepEqual(await version.locator('option').allTextContents(), ['Choose a version…', '27b · 18GB · Text, Image', '27b-mxfp8 · MLX · 19GB · Text']);
  assert.equal(await dialog.locator('input[name^="node_"]').count(), 1);
  assert.equal(await dialog.getByText('Memory fit has not been measured.', { exact: true }).isVisible(), true);
  await dialog.getByRole('checkbox', { name: 'studio-gpu', exact: true }).check();
  await review.click();
  await dialog.getByRole('button', { name: 'Edit selection', exact: true }).click();
  assert.equal(await version.inputValue(), '27b');
  assert.equal(await dialog.getByRole('checkbox', { name: 'studio-gpu', exact: true }).isChecked(), true);
  assert.equal(await dialog.getByRole('button', { name: 'Start download', exact: true }).isVisible(), false);
  await review.click();
  assert.equal(await version.isVisible(), false);
  await dialog.getByText('18GB', { exact: true }).waitFor();
  assert.equal(await dialog.getByText('Text, Image', { exact: true }).isVisible(), true);
  await audit(page, 'ollama-download-review-desktop');
  await page.setViewportSize({ width: 390, height: 844 });
  await audit(page, 'ollama-download-review-phone');
  await dialog.getByRole('button', { name: 'Start download', exact: true }).click();
  await dialog.getByText('model-download', { exact: true }).waitFor();
  const writes = (await f.requests()).filter(call => call.path === '/deployments' && call.method === 'POST');
  assert.deepEqual(writes.map(call => call.payload), [{ model: 'qwen3.8', registry: 'ollama', provider: 'ollama', nodes: ['studio-gpu'], file: '27b' }]);
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
  assert.deepEqual(writes[0].payload, { model: 'example/small-model', registry: 'huggingface', provider: 'vllm', nodes: ['studio-gpu'], file: 'small-Q4.gguf', format: 'gguf' });
});

test('provider plans, parameter scopes and services use the shared side panel', { timeout: 60000 }, async t => {
  const f = await consolePage(t); const { page } = f;
  let jobDone = false;
  await page.route(/\/zzrouter\/v1\/jobs\/install-job\?/, route => route.fulfill({ json: { data: {
    job_id: 'install-job', node: 'studio-gpu', kind: 'install', epoch: 'fixture', seq: jobDone ? 2 : 1, phase: jobDone ? 'done' : 'running',
  } } }));
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
  const resume = page.getByRole('button', { name: 'Return to reviewed plan', exact: true });
  assert.equal(await resume.isEnabled(), false);
  jobDone = true;
  await page.getByRole('dialog').getByRole('button', { name: 'Refresh', exact: true }).click();
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Return to reviewed plan' && !button.disabled));
  await resume.click();
  await page.getByRole('button', { name: 'Run all steps', exact: true }).waitFor();
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
  let releaseStatus;
  const waiting = new Promise(resolve => { releaseStatus = resolve; });
  await page.route('**/update/status?nodes=all', async route => { await waiting; await route.continue(); });
  await f.state('update-invalid'); await f.open('#settings?item=updates', 390, 844);
  await page.getByRole('button', { name: 'Install version', exact: true }).click();
  await page.getByText('Reading current update status…', { exact: true }).waitFor();
  releaseStatus();
  await page.getByLabel('Release version').fill('invalid-release');
  assert.equal(await page.getByLabel('spare-pc', { exact: true }).isDisabled(), true);
  assert.equal(await page.getByText('Update status unavailable', { exact: true }).count(), 1);
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

test('Chat retains its conversation through navigation and starts fresh on a route handoff', { timeout: 45000 }, async t => {
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
  assert.equal(await p.locator('.chat-settings-summary').innerText(), 'Model defaults');
  await p.waitForFunction(() => !document.querySelector('.chat-message'));
  assert.equal(await p.getByRole('textbox', { name: 'Message', exact: true }).inputValue(), '');
  await p.getByRole('textbox', { name: 'Message', exact: true }).fill('Prompt for the new route');
  await p.getByRole('button', { name: 'Send ↑' }).click();
  await p.getByRole('button', { name: 'Regenerate' }).waitFor();
  const calls = (await f.requests()).filter(row => row.path === '/v1/chat/completions');
  assert.equal(calls.length, 2);
  assert.equal(calls[1].payload.model, 'local-chat');
  assert.equal(calls[1].payload.temperature, undefined);
  assert.deepEqual(calls[1].payload.messages, [{ role: 'user', content: 'Prompt for the new route' }]);
  await p.getByRole('button', { name: 'New chat', exact: true }).click();
  await p.waitForFunction(() => !document.querySelector('.chat-message'));
  assert.equal(await p.locator('.chat-message').count(), 0);
  assert.equal(await p.getByRole('textbox', { name: 'Message', exact: true }).inputValue(), '');
  assert.equal(await p.locator('.chat-model-name').innerText(), 'Select a model or route');
});

test('Chat model picker clears the transcript and draft only when selecting a different model', async t => {
  const f = await consolePage(t), p = f.page;
  await f.open('#chat');
  await f.model('coding');
  const composer = p.getByRole('textbox', { name: 'Message', exact: true });
  await composer.fill('First model prompt');
  await p.getByRole('button', { name: 'Send ↑' }).click();
  await p.getByRole('button', { name: 'Regenerate' }).waitFor();
  await composer.fill('First model draft');
  await f.model('coding');
  assert.equal(await composer.inputValue(), 'First model draft');
  assert.equal(await p.locator('.chat-message').count(), 2);
  await f.model('qwen3-coder@studio-gpu');
  await p.waitForFunction(() => !document.querySelector('.chat-message'));
  assert.equal(await composer.inputValue(), '');
  assert.equal(await p.locator('.chat-model-name').innerText(), 'qwen3-coder@studio-gpu');
  await composer.fill('Only the second model gets this');
  await p.getByRole('button', { name: 'Send ↑' }).click();
  await p.getByRole('button', { name: 'Regenerate' }).waitFor();
  const calls = (await f.requests()).filter(row => row.path === '/v1/chat/completions');
  assert.equal(calls.length, 2);
  assert.equal(calls[1].payload.model, 'qwen3-coder@studio-gpu');
  assert.deepEqual(calls[1].payload.messages, [{ role: 'user', content: 'Only the second model gets this' }]);
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
  await p.getByRole('button', { name: 'Settings', exact: true }).click();
  assert.equal(await p.getByRole('textbox', { name: 'System prompt' }).inputValue(), 'Unsaved instruction');
  await p.getByRole('button', { name: 'Close chat settings', exact: true }).click();
  await p.getByRole('button', { name: 'Change', exact: true }).click();
  await search.fill('my-exact-model');
  await p.getByRole('button', { name: 'Use exact ID: my-exact-model', exact: true }).click();
  assert.equal(await p.locator('.chat-model-name').innerText(), 'my-exact-model');
  await p.getByRole('button', { name: 'Settings', exact: true }).click();
  assert.equal(await p.getByRole('textbox', { name: 'System prompt' }).inputValue(), '');
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

test('Overview opens a larger map with locked zoom on desktop and phone', async t => {
  const f = await consolePage(t), { page } = f;
  const canvas = page.locator('.topology-canvas');
  const camera = () => canvas.evaluate(map => {
    const cy = map._cyreg.cy;
    const width = cy.width(), height = cy.height();
    const visible = cy.nodes('[kind="compute"]').filter(node => {
      const box = node.renderedBoundingBox();
      return box.x1 >= 0 && box.x2 <= width && box.y1 >= 0 && box.y2 <= height;
    });
    return { zoom: cy.zoom(), width, height, visible: visible.length, userZooming: cy.userZoomingEnabled() };
  });
  for (const [width, height, minHeight] of [[1440, 900, 560], [390, 844, 500]]) {
    await f.open('#overview', width, height);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const initial = await camera();
    assert.ok(initial.height >= minHeight, JSON.stringify(initial));
    assert.ok(initial.zoom >= 0.75);
    assert.ok(initial.visible >= 1, 'the initial viewport shows a complete compute node');
    assert.equal(initial.userZooming, false);
    assert.equal(await page.getByRole('button', { name: /^(Zoom (in|out)|Fit network to view)$/ }).count(), 0);
    await canvas.scrollIntoViewIfNeeded();
    await canvas.hover();
    await page.mouse.wheel(0, -240);
    assert.equal((await camera()).zoom, initial.zoom);
    if (width < 650) {
      await canvas.scrollIntoViewIfNeeded();
      const area = await canvas.boundingBox();
      const y = Math.round(Math.min(height - 24, Math.max(24, area.y + area.height / 2)));
      const x = Math.round(area.x + area.width / 2);
      const input = await page.context().newCDPSession(page);
      const pinch = (type, spread) => input.send('Input.dispatchTouchEvent', { type, touchPoints:
        spread ? [{ x: x - spread, y, id: 1 }, { x: x + spread, y, id: 2 }] : [] });
      await pinch('touchStart', 30);
      await pinch('touchMove', 75);
      await pinch('touchEnd', 0);
      assert.equal((await camera()).zoom, initial.zoom);
      await input.detach();
    }
    await page.getByRole('button', { name: 'Center map', exact: true }).click();
    assert.equal((await camera()).zoom, initial.zoom);
    await audit(page, 'overview-fixed-map-' + width);
  }
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
  await p.locator('.topology-canvas').waitFor();
  await p.getByRole('button', { name: 'List view', exact: true }).click();
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

for (const method of ['password', 'key', 'temporary code', 'existing session']) {
  test('OAuth continuation returns ' + method + ' to explicit consent', { timeout: 60000 }, async t => {
    const f = await consolePage(t, method === 'existing session' ? 'person' : 'login'), { page } = f;
    const target = '/oauth/authorize?client_id=editor&state=synthetic-oauth-state&code_challenge=synthetic-challenge&redirect_uri=http%3A%2F%2Flocalhost%3A7312%2Fcallback';
    let bootstrapHash;
    await page.route('**/zzrouter/v1/session', async route => {
      if (bootstrapHash === undefined) bootstrapHash = await page.evaluate(() => location.hash);
      await route.continue();
    });
    await page.goto(f.base + '#authorize?request=' + encodeURIComponent(target));
    if (method !== 'existing session') {
      await page.getByRole('heading', { name: 'Sign in to your router', exact: true }).waitFor();
      assert.equal(new URL(page.url()).hash, '');
      assert.equal(new URL(page.url()).search, '');
      assert.doesNotMatch(await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, state: history.state })), /synthetic|authorize|request/);
      if (method === 'password') {
        await audit(page, 'oauth-account-sign-in');
        await page.getByLabel('Username', { exact: true }).fill('alex');
        await page.getByLabel('Password', { exact: true }).fill('synthetic long passphrase');
      } else {
        await page.getByText('Advanced access', { exact: true }).click();
        await page.getByRole('button', { name: method === 'key' ? 'Use an access key' : 'Use a temporary code', exact: true }).click();
        await page.getByLabel(method === 'key' ? 'Router API key' : 'Temporary code', { exact: true }).fill(method === 'key' ? 'synthetic-legacy-key' : 'synthetic-session-code');
      }
      await page.getByRole('button', { name: method === 'temporary code' ? 'Continue' : 'Sign in', exact: true }).click();
    }
    await page.getByRole('heading', { name: 'Review connection', exact: true }).waitFor();
    assert.equal(page.url(), f.origin + target);
    assert.equal(bootstrapHash, '', 'the continuation fragment is scrubbed before session bootstrap');
    const oauth = (await f.requests()).filter(call => call.path === '/oauth/authorize');
    assert.deepEqual(oauth.map(call => call.method), ['GET'], 'authentication must never submit consent');
    assert.equal(oauth[0].query.state, 'synthetic-oauth-state');
    assert.equal(oauth[0].query.redirect_uri, 'http://localhost:7312/callback');
    assert.equal(await page.getByRole('button', { name: 'Allow connection', exact: true }).isVisible(), true);
    assert.doesNotMatch(await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, state: history.state })), /synthetic|authorize|request/);
  });
}

test('OAuth continuation rejects attacker destinations and is not retained across refresh', { timeout: 60000 }, async t => {
  const f = await consolePage(t, 'person'), { page } = f;
  const external = [];
  let navigation = 0;
  await page.route('https://attacker.example/**', route => { external.push(route.request().url()); return route.abort(); });
  for (const target of [
    'https://attacker.example/oauth/authorize?client_id=bad', '//attacker.example/oauth/authorize?client_id=bad',
    '/oauth/authorize?client_id=bad#hidden', '/oauth/authorize/../connections?client_id=bad',
  ]) {
    await page.goto(f.base + '?case=' + ++navigation + '#authorize?request=' + encodeURIComponent(target));
    await page.getByRole('heading', { name: 'Overview', exact: true }).waitFor();
    assert.equal(new URL(page.url()).origin, f.origin);
    assert.doesNotMatch(page.url(), /client_id|attacker|authorize|request=/);
  }
  assert.deepEqual(external, []);
  assert.equal((await f.requests()).filter(call => call.path === '/oauth/authorize').length, 0);
  await f.state('login');
  await page.goto(f.base + '?case=' + ++navigation + '#authorize?request=' + encodeURIComponent('/oauth/authorize?client_id=editor&state=synthetic-state'));
  await page.getByLabel('Username', { exact: true }).waitFor();
  await page.reload();
  await page.getByLabel('Username', { exact: true }).fill('alex');
  await page.getByLabel('Password', { exact: true }).fill('synthetic long passphrase');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await page.getByRole('heading', { name: 'Overview', exact: true }).waitFor();
  assert.equal((await f.requests()).filter(call => call.path === '/oauth/authorize').length, 0, 'refresh discards the in-memory continuation');
});

for (const entry of ['nodes', 'overview']) test(`an empty owner can add a worker from ${entry} and prepare and test its exact model`, { timeout: 120000 }, async t => {
  const f = await consolePage(t, 'person'), { page } = f;
  let paired = false, installed = false, downloaded = false, verified = false;
  let pairedReads = 0;
  const calls = [], worker = { name: 'new-worker', os: 'Linux', health_status: 'healthy', providers: [] };
  const model = { name: 'example/small-model', node: worker.name, assigned_app: 'vllm' };
  await page.route('**/zzrouter/v1/**', async route => {
    const req = route.request(), url = new URL(req.url()), path = url.pathname.replace('/zzrouter/v1', '');
    const body = req.postDataJSON() || {};
    calls.push({ path, method: req.method(), body, node: url.searchParams.get('node') });
    const send = data => route.fulfill({ json: { data } });
    if (path === '/access/onboarding') return send({ provider_ready: installed, model_ready: downloaded, response_verified: verified });
    if (path === '/nodes') return send([{ name: 'demo-coordinator', os: 'macOS', health_status: 'healthy', providers: [] },
      ...(paired && pairedReads++ > 0 ? [{ ...worker, providers: installed ? [{ key: 'vllm', kind: 'on-demand', state: 'live', run_lifecycle: 'process' }] : [] }] : [])]);
    if (path === '/nodes/compatible') return route.fulfill({ json: { data: [], metadata: { assessments:
      installed ? [{ node: worker.name, provider: 'vllm', status: 'unknown' }] : [] } } });
    if (path === '/providers') return send(installed ? [{ name: 'vllm', node: worker.name, kind: 'python', enabled: true, installed: true, state: 'live' }] : []);
    if (path === '/models') return send(downloaded && (!url.searchParams.has('node') || url.searchParams.get('node') === worker.name) ? [model] : []);
    if (path === '/cluster/pairing/accept') { paired = true; return route.fulfill({ json: { node_name: worker.name } }); }
    if (path === '/providers/vllm' && req.method() === 'GET') return send({ name: 'vllm', node: worker.name, enabled: installed, installed, state: installed ? 'live' : 'dormant' });
    if (path === '/providers/vllm/install') { installed = true; return send({ accepted: true, node: worker.name, provider: 'vllm' }); }
    if (path === '/deployments' && req.method() === 'POST' || path === '/deployments/new-worker-download') {
      downloaded = true;
      return send({ id: 'new-worker-download', model: model.name, status: 'completed', nodes_total: 1, nodes_complete: 1, nodes: [{ node: worker.name, status: 'completed', progress: 100 }] });
    }
    return route.continue();
  });
  await page.route('**/v1/models', route => route.fulfill({ json: { data: downloaded ? [{ id: model.name + '@' + worker.name, capabilities: { chat: true }, zzrouter: { node: worker.name, provider: 'vllm' } }] : [] } }));
  await page.route('**/v1/chat/completions', async route => {
    calls.push({ path: '/v1/chat/completions', node: route.request().headers()['x-node'], body: route.request().postDataJSON() });
    verified = true;
    await route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream', 'X-zzrouter-Node': worker.name, 'X-zzrouter-Provider': 'vllm' }, body: 'data: {"choices":[{"delta":{"content":"Worker is ready."},"finish_reason":null}]}\n\ndata: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n' });
  });
  await f.open('#overview');
  assert.match(await page.getByRole('region', { name: 'Getting started' }).innerText(), /Additional compute nodes are optional/);
  await page.getByRole('button', { name: 'Skip setup for now', exact: true }).click();
  await f.nav(entry === 'nodes' ? 'Nodes' : 'Overview');
  await page.getByRole('button', { name: '+ Add compute node', exact: true }).click();
  for (const os of ['darwin', 'windows', 'linux']) {
    await page.getByLabel('Operating system', { exact: true }).selectOption(os);
    await page.getByLabel('Connect to this router', { exact: true }).waitFor();
    assert.match(await page.getByLabel('Install as a worker', { exact: true }).inputValue(), /worker/);
    assert.doesNotMatch(await page.getByRole('dialog').innerText(), /\bnull\b/);
  }
  await audit(page, 'add-worker-guide-desktop');
  await page.setViewportSize({ width: 390, height: 844 });
  await audit(page, 'add-worker-guide-phone');
  await page.getByRole('button', { name: 'I have pairing details', exact: true }).click();
  await page.getByLabel('Pairing details', { exact: true }).fill(JSON.stringify({ type: 'zzrouter-pairing', version: 1, node_name: worker.name, code: 'synthetic-only', worker_url: 'https://new-worker.example:9091', worker_fingerprint: 'sha256:synthetic-only', expires_at: '2030-01-01T00:00:00Z' }));
  await page.getByRole('button', { name: 'Review computer', exact: true }).click();
  await page.getByRole('button', { name: 'Approve computer', exact: true }).click();
  await page.getByRole('button', { name: 'Refresh nodes', exact: true }).waitFor();
  assert.equal(await page.getByRole('button', { name: 'View node', exact: true }).count(), 0, 'Do not open missing node details while the first observation is pending');
  await page.getByRole('button', { name: 'Refresh nodes', exact: true }).click();
  if (entry === 'overview') await page.getByRole('button', { name: 'List view', exact: true }).click();
  await page.locator('#content button').filter({ hasText: 'new-worker' }).click();
  await page.getByText('Provider setup needed', { exact: true }).waitFor();
  assert.doesNotMatch(await page.getByRole('dialog').innerText(), /null/, 'An unprepared node must not render a missing readiness hint');
  await page.getByRole('button', { name: 'Set up a provider', exact: true }).click();
  assert.match(page.url(), /node=new-worker/);
  await page.reload();
  await page.getByText('Preparing models on new-worker', { exact: true }).waitFor();
  await page.locator('[data-provider="vllm"]').click();
  assert.equal(await page.getByLabel('Compute node', { exact: true }).inputValue(), worker.name);
  assert.equal(await page.getByLabel('Compute node', { exact: true }).isDisabled(), true);
  await page.goBack();
  await page.getByRole('dialog').waitFor({ state: 'hidden' });
  assert.match(page.url(), /node=new-worker/);
  await page.goForward();
  assert.equal(await page.getByLabel('Compute node', { exact: true }).inputValue(), worker.name);
  await page.getByRole('button', { name: 'Manage runtime', exact: true }).click();
  await page.getByRole('button', { name: 'Install or repair', exact: true }).click();
  await page.getByRole('button', { name: 'Review install plan', exact: true }).click();
  await page.getByRole('button', { name: 'Run all steps', exact: true }).click();
  await page.getByRole('button', { name: 'Inspect provider', exact: true }).click();
  await page.getByRole('button', { name: 'Choose a model', exact: true }).waitFor();
  assert.match(await page.locator('[data-provider="vllm"]').innerText(), /1 enabled/, 'The provider card and its inspected runtime must use fresh inventory');
  await page.getByRole('button', { name: 'Choose a model', exact: true }).click();
  await page.getByRole('button', { name: /Small model/ }).click();
  await page.getByRole('button', { name: 'Download', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('[name=download_provider]')?.value === 'vllm');
  assert.equal(await page.getByLabel('Local provider', { exact: true }).inputValue(), 'vllm');
  assert.equal(await page.getByLabel(worker.name, { exact: true }).count(), 0);
  await page.getByLabel('Weights variant', { exact: true }).selectOption('small-Q4.gguf');
  await page.getByLabel(worker.name, { exact: true }).waitFor();
  assert.equal(await page.getByLabel(worker.name, { exact: true }).isChecked(), true);
  assert.equal(await page.getByLabel('demo-coordinator', { exact: true }).count(), 0);
  await page.getByRole('button', { name: 'Review download', exact: true }).click();
  await page.getByRole('button', { name: 'Start download', exact: true }).click();
  await page.getByRole('button', { name: 'Inspect available models', exact: true }).click();
  assert.equal(await page.getByLabel('Compute node', { exact: true }).inputValue(), worker.name);
  await page.getByRole('button', { name: model.name, exact: true }).click();
  await page.getByRole('button', { name: 'Chat', exact: true }).last().click();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  assert.equal(await page.getByLabel('Compute node', { exact: true }).isDisabled(), true);
  assert.equal(await page.getByLabel('Compute node', { exact: true }).inputValue(), worker.name);
  await page.getByRole('button', { name: 'Close chat settings', exact: true }).click();
  await page.getByRole('textbox', { name: 'Message', exact: true }).fill('Say hello');
  await page.getByRole('button', { name: 'Send ↑', exact: true }).click();
  await page.getByText('Worker is ready.', { exact: true }).waitFor();
  assert.equal(calls.find(call => call.path === '/providers/vllm/install').node, worker.name);
  assert.deepEqual(calls.find(call => call.path === '/deployments' && call.method === 'POST').body.nodes, [worker.name]);
  assert.equal(calls.find(call => call.path === '/v1/chat/completions').node, worker.name);
  const setupStatus = page.waitForResponse(response => new URL(response.url()).pathname === '/zzrouter/v1/access/onboarding');
  await f.nav('Overview');
  await setupStatus;
  await page.getByRole('region', { name: 'Getting started', exact: true }).waitFor({ state: 'detached' });
  assert.equal(await page.getByRole('heading', { name: 'Your router is ready', exact: true }).count(), 0);
  assert.equal(await page.getByRole('button', { name: '+ Add compute node', exact: true }).count(), 1);
  assert.equal(await page.getByRole('button', { name: 'Add compute node', exact: true }).count(), 0);
  assert.equal(await page.getByRole('button', { name: 'Skip setup for now', exact: true }).count(), 0);
  assert.equal(await page.getByRole('button', { name: 'Dismiss', exact: true }).count(), 0);
  await audit(page, 'optional-worker-ready-phone');
  paired = false; downloaded = false;
  await f.open('#models?node=new-worker');
  assert.equal(await page.getByLabel('Compute node', { exact: true }).inputValue(), worker.name);
  assert.equal(await page.getByLabel('Compute node', { exact: true }).isDisabled(), true);
  assert.equal(await page.getByRole('button', { name: model.name, exact: true }).count(), 0);
  await f.open('#models?node=new-worker&provider=vllm&mode=discover');
  await page.getByRole('button', { name: /Small model/ }).click();
  await page.getByRole('button', { name: 'Download', exact: true }).click();
  await page.getByText('new-worker is not in the current inventory. Reconnect it before downloading a model.', { exact: true }).waitFor();
  assert.equal(await page.getByRole('button', { name: 'Review download', exact: true }).isDisabled(), true);
});

test('entering worker Chat clears a different target even without a model selection', { timeout: 30000 }, async t => {
  const f = await consolePage(t), { page } = f;
  await f.open('#chat?item=qwen3-coder%40studio-gpu&mode=studio-gpu');
  await page.evaluate(() => { location.hash = '#chat?node=new-worker'; });
  await page.getByText('Preparing models on new-worker', { exact: true }).waitFor();
  assert.equal(await page.locator('.chat-model-name').innerText(), 'Select a model or route');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  assert.equal(await page.getByLabel('Compute node', { exact: true }).inputValue(), 'new-worker');
  assert.equal(await page.getByLabel('Compute node', { exact: true }).isDisabled(), true);
});


test('Chat keeps short replies readable without clipping its header or composer', { timeout: 120000 }, async t => {
  const f = await consolePage(t), { page } = f;
  await page.route('**/v1/chat/completions', route => route.fulfill({
    contentType: 'text/event-stream',
    headers: { 'X-zzrouter-Provider': 'ollama', 'X-zzrouter-Node': 'fixture-worker' },
    body: 'data: {"choices":[{"delta":{"content":"I am your assistant. How can I help?"},"finish_reason":"stop"}]}\n\ndata: {"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":22,"total_tokens":53},"timings":{"prompt_ms":300,"predicted_ms":3500,"predicted_per_second":6.3}}\n\ndata: [DONE]\n\n',
  }));
  for (const [width, height] of [[1440, 900], [1366, 768], [1135, 696], [1280, 500], [390, 844], [320, 568], [390, 420]]) {
    await f.open('#chat', width, height); await f.model('coding');
    await page.getByRole('textbox', { name: 'Message', exact: true }).fill("What's your name?");
    await page.getByRole('button', { name: 'Send ↑', exact: true }).click();
    const stats = page.locator('.chat-assistant .chat-stats');
    await stats.waitFor();
    assert.match(await stats.innerText(), /^31 input tokens · 22 output tokens · Prompt processing 0\.30s · Generation 3\.50s · 6\.3 tok\/s · \d+\.\d{2}s total\* · \* Measured or calculated in the browser$/);
    const layout = await page.evaluate(() => {
      const rect = selector => {
        const r = document.querySelector(selector).getBoundingClientRect();
        return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, height: r.height };
      };
      const viewport = document.querySelector('.chat-viewport');
      return { toolbar: rect('.chat-toolbar'), model: rect('.chat-model-name'), viewport: rect('.chat-viewport'),
        user: rect('.chat-user'), assistant: rect('.chat-assistant'), composer: rect('.chat-composer'),
        scrollTop: viewport.scrollTop, scrollHeight: viewport.scrollHeight, clientHeight: viewport.clientHeight };
    });
    const name = `chat-short-reply-${width}-${height}`;
    await writeFile(join(artifacts, name + '-layout.json'), JSON.stringify(layout, null, 2));
    await audit(page, name);
    assert.ok(layout.toolbar.bottom <= layout.viewport.top + 1, name + ': header must not overlap transcript');
    assert.ok(layout.viewport.bottom <= layout.composer.top + 1, name + ': composer must not cover transcript');
    assert.ok(Math.abs(layout.assistant.left - layout.composer.left) <= 1, name + ': reply and composer share a left edge');
    assert.ok(Math.abs(layout.assistant.left - layout.model.left) <= 1, name + ': reply and model share a left edge');
    assert.ok(layout.viewport.height >= 100, name + ': transcript remains usable');
    if (width >= 650 && height >= 650) {
      assert.ok(layout.user.top >= layout.viewport.top, name + ': short user message must be fully visible');
      assert.ok(layout.assistant.bottom <= layout.viewport.bottom + 1, name + ': short reply and actions must be fully visible');
    }
  }
});

test('Chat distinguishes reported timings, calculated speed, and browser timing', { timeout: 30000 }, async t => {
  const f = await consolePage(t), { page } = f;
  const timingFrames = [
    { timings: { prompt_ms: 300, predicted_ms: 3500 } },
    { timings: { prompt_ms: -1, predicted_ms: null } },
    { metrics: { time_to_first_token_ms: 85.2, generation_time_ms: 1240.5, tokens_per_second: 103.2 } },
  ];
  let replies = 0;
  await page.route('**/v1/chat/completions', route => {
    const stats = { choices: [], usage: { prompt_tokens: 31, completion_tokens: 22, total_tokens: 53 }, ...timingFrames[replies++] };
    return route.fulfill({ contentType: 'text/event-stream', body:
      'data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":"stop"}]}\n\n' +
      `data: ${JSON.stringify(stats)}\n\ndata: [DONE]\n\n` });
  });
  await f.open('#chat'); await f.model('coding');
  const composer = page.getByRole('textbox', { name: 'Message', exact: true });
  const send = page.getByRole('button', { name: 'Send ↑', exact: true });
  const stats = page.locator('.chat-assistant .chat-stats');
  await composer.fill('First reply'); await send.click();
  await stats.nth(0).waitFor();
  assert.match(await stats.nth(0).innerText(), /Prompt processing 0\.30s · Generation 3\.50s · 6\.3 tok\/s\* ·/);
  await composer.fill('Second reply'); await send.click();
  await stats.nth(1).waitFor();
  assert.match(await stats.nth(1).innerText(), /^31 input tokens · 22 output tokens · First token \d+\.\d{2}s\* · Output stream \d+\.\d{2}s\* · \d+\.\d{2}s total\* · \* Measured or calculated in the browser$/);
  await composer.fill('Third reply'); await send.click();
  await stats.nth(2).waitFor();
  assert.match(await stats.nth(2).innerText(), /First token 0\.09s · Generation 1\.24s · 103\.2 tok\/s ·/);
});

test('Chat confines long replies, code and multiline drafts to their scroll regions', { timeout: 90000 }, async t => {
  const f = await consolePage(t), { page } = f;
  const content = Array.from({ length: 12 }, (_, i) => `Paragraph ${i + 1}. A longer response stays inside the conversation.`).join('\n\n') + '\n\n```text\n' + 'wide_code_value_'.repeat(30) + '\n```';
  await page.route('**/v1/chat/completions', route => route.fulfill({ contentType: 'text/event-stream',
    body: 'data: ' + JSON.stringify({ choices: [{ delta: { content }, finish_reason: 'stop' }] }) + '\n\ndata: [DONE]\n\n',
  }));
  for (const [width, height] of [[1280, 800], [390, 844], [390, 420]]) {
    await f.open('#chat', width, height); await f.model('coding');
    const input = page.getByRole('textbox', { name: 'Message', exact: true });
    await input.fill('Show a longer example');
    await page.getByRole('button', { name: 'Send ↑', exact: true }).click();
    await page.locator('.chat-assistant pre').waitFor();
    await page.locator('.chat-viewport').evaluate(viewport => { viewport.scrollTop = 0; });
    const latest = page.getByRole('button', { name: 'Latest message ↓', exact: true });
    await latest.click();
    await input.fill(Array.from({ length: 18 }, (_, i) => `Draft line ${i + 1}`).join('\n'));
    const regions = await page.evaluate(() => {
      const viewport = document.querySelector('.chat-viewport'), input = document.querySelector('.chat-composer textarea');
      const code = document.querySelector('.chat-markdown pre');
      return { scrolling: viewport.scrollHeight > viewport.clientHeight, draftScrolling: input.scrollHeight > input.clientHeight,
        codeScrolling: code.scrollWidth > code.clientWidth, height: viewport.getBoundingClientRect().height };
    });
    assert.equal(regions.scrolling, true, 'Long conversations scroll independently');
    assert.equal(regions.draftScrolling, true, 'Long drafts scroll inside the composer');
    assert.equal(regions.codeScrolling, true, 'Wide code scrolls inside its block');
    assert.ok(regions.height >= 40, 'A long draft leaves the transcript visible');
    assert.match(await input.inputValue(), /Draft line 18/);
    await audit(page, `chat-long-reply-draft-${width}-${height}`);
  }
});

test('client guidance, team impact, access history and recovery fit desktop and phone', { timeout: 60000 }, async t => {
  const f = await consolePage(t, 'person'), { page } = f;
  for (const width of [1280, 390]) {
    const height = width === 390 ? 844 : 800;
    await f.open('#clients?item=connect', width, height);
    await page.getByText('This computer only', { exact: true }).waitFor();
    await page.getByText('Management MCP endpoint', { exact: true }).waitFor();
    await audit(page, 'connection-guide-' + width);
    await f.open('#access?item=teams%2Fengineering', width, height);
    await page.getByRole('dialog').getByText('2 people · 2 app keys · 0 pending invitations', { exact: true }).waitFor();
    await audit(page, 'team-impact-' + width);
    await f.open('#settings?item=audit', width, height);
    await page.getByText('1 event shown.', { exact: true }).waitFor();
    await page.getByRole('button', { name: 'Load older', exact: true }).click();
    await page.getByText('2 events shown.', { exact: true }).waitFor();
    assert.doesNotMatch(await page.locator('#content').innerText(), /(?:^|\n)null(?:\n|$)/);
    const tabColors = await page.locator('.tabs').evaluate(tabs => [getComputedStyle(tabs.querySelector('[aria-current]')).backgroundColor, getComputedStyle(tabs.querySelector('button:not([aria-current])')).backgroundColor]);
    assert.notEqual(tabColors[0], tabColors[1], 'The selected tab has a visible state');
    await audit(page, 'access-history-' + width);
    await f.open('#settings?item=recovery', width, height);
    await page.getByText('Installation recovery', { exact: true }).waitFor();
    assert.match(await page.getByLabel('Create a backup', { exact: true }).inputValue(), /recovery backup --dir/);
    await audit(page, 'recovery-scope-' + width);
  }
});
