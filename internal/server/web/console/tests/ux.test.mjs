import test from 'node:test';
import assert from 'node:assert/strict';
import { el, input, formFeedback, invalidField, problem } from '../dom.mjs';
import { createChatSession, defaultSettings } from '../chat.mjs';
import { providerObservationSummary } from '../inventory.mjs';
import { installDOM, walk } from './dom-fixture.mjs';

test('form feedback maps server fields explicitly and preserves existing descriptions', t => {
  const root = installDOM(t);
  const control = input('ui-alias', 'value', { 'aria-describedby': 'original-hint' });
  const other = input('server.path', 'other');
  const feedback = el('div'), form = el('form', {}, feedback, control, other);
  root.append(form);
  const validation = formFeedback(form, feedback, key => key === 'server.path' ? control : undefined);
  validation.error({ message: 'Check your input.', errors: [{ key: 'server.path', message: 'Choose a different value.' }] });
  assert.equal(control.getAttribute('aria-invalid'), 'true');
  assert.match(control.getAttribute('aria-describedby'), /^original-hint form-error-/);
  assert.equal(other.getAttribute('aria-invalid'), undefined, 'UI names do not implicitly map API paths');
  assert.equal(document.activeElement, feedback);
  validation.error({ message: 'Another error.', errors: [{ key: 'unknown.path', message: 'Unknown field.' }] });
  assert.equal(control.getAttribute('aria-invalid'), undefined);
  assert.equal(control.getAttribute('aria-describedby'), 'original-hint');
  validation.error(invalidField('Choose a value.', control));
  assert.equal(control.getAttribute('aria-invalid'), 'true');
  form.dispatchEvent(new Event('input'));
  assert.equal(control.getAttribute('aria-describedby'), 'original-hint');
});

test('uncertain and applied outcomes stay prominent while technical context is disclosed separately', t => {
  installDOM(t);
  const root = problem({ message: 'Response interrupted.', uncertain: true, mutation_applied: true, code: 'write_failed', request_id: 'sample-id' });
  const technical = walk(root).find(node => node.tag === 'details');
  assert.match(root.childNodes[0].textContent, /Do not resubmit automatically/);
  assert.ok(root.children.some(node => node.tag === 'p' && /change was applied/.test(node.textContent)));
  assert.match(technical.textContent, /write_failed.*sample-id/s);
  assert.equal(technical.getAttribute('open'), undefined);
});

test('a late Chat SDK load cannot mount after its browser authority is cleared', async t => {
  installDOM(t);
  let resolve, mounted = 0;
  const authority = new AbortController();
  const session = createChatSession({ authority, origin: 'https://router.example', inferenceFetch() {}, canChange: () => true },
    () => new Promise(done => { resolve = done; }));
  session.settings = { ...defaultSettings(), model: 'private-model', system: 'private-instruction' };
  authority.abort();
  resolve({ mountChat() { mounted++; } });
  await session.ready;
  assert.equal(mounted, 0);
  assert.deepEqual(session.settings, defaultSettings());
  assert.equal(session.view, undefined);
});

test('provider summaries report observations without treating credentials as health', () => {
  const ok = value => ({ status: 'fulfilled', value });
  const entry = { key: 'cloud', available: true };
  const inventory = [ok([{ name: 'worker', health_status: 'down', providers: [{ key: 'cloud' }] }]), ok([{ name: 'cloud', node: 'worker', enabled: false }])];
  assert.equal(providerObservationSummary(entry, inventory), '1 reported node · 0 enabled · 1 node down');
  assert.equal(providerObservationSummary(entry, [inventory[0], { status: 'rejected' }]), '1 reported node · Enablement unavailable · 1 node down');
  assert.doesNotMatch(providerObservationSummary(entry, [ok([]), ok([])]), /healthy|connected/i);
});
