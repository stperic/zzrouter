import test from 'node:test';
import assert from 'node:assert/strict';
import { providers } from '../providers.mjs';
import { find, named, button, drain, installDOM } from './dom-fixture.mjs';

for (const [provider, isCloud, action, activate] of [
  ['openai', true, 'Save credentials', false],
  ['openai', true, 'Save and connect', true],
  ['huggingface', false, 'Save credentials', false],
]) {
  test(`${provider} ignores implicit submission while explicit ${action} remains available without testing`, async t => {
    const root = installDOM(t);
    const writes = [];
    const controller = new AbortController();
    t.after(() => controller.abort());
    const status = { managed_available: true, revision: 7, scope: 'coordinator', mode: 'managed', fields: [{ id: 'token', label: 'Access token', type: 'text', required: isCloud, secret: true }] };
    await providers({
      root, current: () => true, scope: { signal: controller.signal }, syncActivity() {},
      api: {
        canLeave: () => true, canChange: () => true,
        list: async () => [{ key: provider, name: provider, is_cloud: isCloud, connection_supported: true }],
        data: async (path, options = {}) => {
          if (!options.method) return status;
          writes.push({ path, ...options });
          return { saved: true, durable: true, active: activate, published: true };
        },
      },
    });
    named(root, 'token').value = 'synthetic-candidate';
    const form = find(root, node => node.tag === 'form');
    const submit = new Event('submit', { cancelable: true });
    form.dispatchEvent(submit);
    await drain();
    assert.equal(submit.defaultPrevented, true);
    assert.deepEqual(writes, [], 'pressing Enter in the single credential field cannot change the connection');
    assert.equal(named(root, 'token').value, 'synthetic-candidate');
    const save = button(root, action);
    assert.equal(save.type, 'button', 'explicit keyboard activation uses the button click action');
    save.dispatchEvent(new Event('click'));
    await drain();
    assert.equal(writes.length, 1);
    assert.equal(writes[0].method, 'PUT');
    assert.equal(writes[0].path, `/providers/${provider}/connection`);
    assert.deepEqual(writes[0].body, { expected_revision: 7, values: { token: 'synthetic-candidate' }, activate });
    assert.equal(named(root, 'token').value, '', 'the settled explicit save clears the transient candidate');
  });
}

for (const outcome of ['saved', 'saved_activation_failed']) {
  test(`provider result preserves ${outcome} even when current state is active`, async t => {
    const root = installDOM(t);
    const controller = new AbortController();
    t.after(() => controller.abort());
    await providers({
      root, current: () => true, scope: { signal: controller.signal }, syncActivity() {},
      api: {
        canLeave: () => true, canChange: () => true,
        list: async () => [{ key: 'openai', name: 'OpenAI', is_cloud: true, connection_supported: true }],
        data: async (_path, options = {}) => options.method
          ? { saved: true, durable: true, active: true, published: true, outcome }
          : { managed_available: true, revision: 7, scope: 'coordinator', mode: 'managed', fields: [{ id: 'token', label: 'Token', type: 'text', required: true, secret: true }] },
      },
    });
    named(root, 'token').value = 'synthetic-candidate';
    button(root, 'Save and connect').dispatchEvent(new Event('click'));
    await drain();
    const result = find(root, node => node.className?.includes('notice') && node.textContent.includes('catalog published'));
    assert.ok(result);
    assert.match(result.textContent, /connection active/);
    if (outcome === 'saved_activation_failed') {
      assert.match(result.textContent, /activation reported an error/);
      assert.match(result.className, /warning/);
    } else {
      assert.doesNotMatch(result.textContent, /reported an error/);
      assert.doesNotMatch(result.className, /warning/);
    }
    assert.equal(named(root, 'token').value, '', 'partial activation must not retain candidate credentials');
    assert.equal(button(root, 'Save and connect').disabled, true, 'settled writes require reload before another attempt');
  });
}
