import test from 'node:test';
import assert from 'node:assert/strict';
import { details, formSubmission } from '../details.mjs';
import { el, editorActions } from '../dom.mjs';
import { updates } from '../updates.mjs';
import { installDOM, find, named, button, drain } from './dom-fixture.mjs';

function fixture(t, data = async () => ({})) {
  const root = installDOM(t), abort = new AbortController();
  t.after(() => abort.abort());
  const p = { root, scope: { signal: abort.signal }, current: () => !abort.signal.aborted,
    state: { node_name: 'test' }, go() {}, syncActivity() {}, api: { data, canLeave: () => true, canChange: () => true } };
  return { root, p, abort, owner: () => details(p, 'Test', () => {}) };
}

test('editor submission excludes double sends and preserves drafts after definite refusal', async t => {
  const f = fixture(t), p = f.owner();
  const form = el('form'), controls = el('fieldset'), save = el('button');
  const draft = p.trackDraft(form), submission = formSubmission(p, controls, save, draft);
  draft.touch(); let reject, writes = 0;
  const write = () => { writes++; return new Promise((_, fail) => { reject = fail; }); };
  const saved = () => assert.fail('refused write cannot be saved');
  const first = submission.run(write, saved, (error, blocked) => { assert.equal(error.status, 400); assert.equal(blocked, false); });
  await submission.run(write, saved, assert.fail);
  assert.equal(writes, 1); assert.equal(controls.disabled, true);
  reject(Object.assign(new Error('Refused'), { status: 400 })); await first;
  assert.equal(draft.dirty, true); assert.equal(controls.disabled, false); assert.equal(save.disabled, false);
});

test('a successful write stays blocked if its subsequent observation fails', async t => {
  const f = fixture(t), p = f.owner(), form = el('form'), controls = el('fieldset'), save = el('button');
  const draft = p.trackDraft(form), submission = formSubmission(p, controls, save, draft);
  draft.touch(); let writes = 0;
  await submission.run(async () => { writes++; }, async () => { throw new Error('Read failed'); }, (_, blocked) => assert.equal(blocked, true));
  assert.equal(draft.dirty, false); assert.equal(save.disabled, true);
  await submission.run(async () => { writes++; }, assert.fail, assert.fail);
  assert.equal(writes, 1);
});

test('an obsolete editor cannot render a late submission result', async t => {
  const f = fixture(t), p = f.owner(), form = el('form'), controls = el('fieldset'), save = el('button');
  const submission = formSubmission(p, controls, save, p.trackDraft(form));
  let resolve;
  const pending = submission.run(() => new Promise(done => { resolve = done; }), assert.fail, assert.fail);
  p.panel.view('New selection'); resolve({}); await pending;
});

test('cancelling a nested editor preserves its parent draft', async t => {
  const f = fixture(t), p = f.owner(), primary = el('form'), nested = el('form');
  const mainDraft = p.trackDraft(primary), nestedDraft = p.trackDraft(nested);
  mainDraft.touch(); nestedDraft.touch(); let returned = false, confirmations = 0;
  globalThis.confirm = () => { confirmations++; return true; };
  nested.append(editorActions(el('button'), () => { returned = true; nestedDraft.dispose(); }, p.api, nestedDraft));
  button(nested, 'Cancel').dispatchEvent(new Event('click'));
  assert.equal(returned, true); assert.equal(confirmations, 1); assert.equal(mainDraft.dirty, true);
  globalThis.confirm = () => false;
  assert.equal(p.panel.confirmLeave(), false);
});

test('schedule cancellation respects discard choice and keeps the edited value', async t => {
  const f = fixture(t, async path => {
    if (path.startsWith('/update/settings')) return { enabled: false };
    if (path.startsWith('/update/history')) return { nodes: [{ node: 'test', history: { entries: [] } }] };
    return { nodes: [{ node: 'test', status: { current_version: 'v1', state: 'idle' } }] };
  });
  await updates(f.p, 'test');
  button(f.root, 'Edit update schedule').dispatchEvent(new Event('click')); await drain();
  named(f.root, 'enabled').checked = true;
  find(f.root, node => node.tag === 'form').dispatchEvent(new Event('change'));
  let discard = false, confirmations = 0;
  globalThis.confirm = () => { confirmations++; return discard; };
  button(f.root, 'Cancel').dispatchEvent(new Event('click')); await drain();
  assert.equal(confirmations, 1); assert.equal(named(f.root, 'enabled').checked, true);
  discard = true; button(f.root, 'Cancel').dispatchEvent(new Event('click')); await drain();
  assert.equal(confirmations, 2); assert.match(f.root.textContent, /Installed version/);
});
