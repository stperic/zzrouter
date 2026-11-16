import { Lifetime } from './api.mjs';
import { el, leaveButton, clearSecrets, message, problem } from './dom.mjs';

// The page owns the panel; each details/editor transition owns its reads and secrets.
export function details(p, title, close) {
  const lifetime = new Lifetime();
  const scope = lifetime.next();
  const contents = new Lifetime();
  const drafts = new Set();
  const signal = AbortSignal.any([p.scope.signal, scope.signal]);
  const panel = el('dialog', { class: 'detail-panel', 'aria-label': title });
  const body = el('div', { class: 'detail-body' });
  const label = el('span', {}, title);
  const previousFocus = document.activeElement;
  const dismiss = leaveButton('×', close, p.api, 'detail-dismiss');
  panel.append(el('div', { class: 'detail-toolbar' }, label, dismiss), body);
  p.root.classList.add('has-details');
  p.root.append(panel);
  let disposed = false;
  let editing = false;
  let revision = 0;
  const dirty = () => [...drafts].some(draft => draft.dirty);
  const confirmLeave = () => p.api.canLeave() && (!dirty() || confirm('Discard unsaved changes?'));
  const unregister = p.guardLeave?.(confirmLeave, dirty);
  const dispose = (restoreFocus = false) => {
    if (disposed) return;
    disposed = true;
    lifetime.end(); contents.end(); drafts.clear(); unregister?.();
    clearSecrets(panel);
    panel.close?.(); panel.remove();
    p.root.classList.remove('has-details');
    if (restoreFocus) {
      const target = previousFocus?.isConnected && previousFocus !== document.body && previousFocus !== document.documentElement ? previousFocus : p.root.querySelector?.('[aria-current="true"] button, button[aria-current="true"]') || document.querySelector?.('h1');
      target?.focus({ preventScroll: true });
    }
  };
  const current = () => !disposed && p.current() && scope.current();
  const trackDraft = form => {
    editing = true;
    const draft = { dirty: false, touch: () => { draft.dirty = true; revision++; }, clean: () => { draft.dirty = false; },
      discard: () => p.api.canLeave() && (!draft.dirty || confirm('Discard unsaved changes?')),
      dispose: () => drafts.delete(draft) };
    drafts.add(draft);
    form.addEventListener('input', draft.touch);
    form.addEventListener('change', draft.touch);
    return draft;
  };
  const view = (nextTitle = title) => {
    const content = contents.next(); revision++;
    const restoreHeader = body.contains(document.activeElement);
    clearSecrets(body); body.replaceChildren(); drafts.clear(); editing = false;
    label.textContent = nextTitle;
    panel.setAttribute('aria-label', nextTitle);
    dismiss.setAttribute('aria-label', `Close ${nextTitle.toLowerCase()}`);
    if (restoreHeader) dismiss.focus({ preventScroll: true });
    const owner = { ...p, root: body, panel: controller, close, dispose, trackDraft,
      scope: { signal: AbortSignal.any([signal, content.signal]) }, current: () => current() && content.current(),
      get editing() { return editing; }, set editing(value) { editing = value; } };
    return owner;
  };
  const controller = { current, view, dispose, confirmLeave, signal, get revision() { return revision; }, get editing() { return editing; } };
  signal.addEventListener('abort', () => dispose(), { once: true });
  panel.addEventListener('cancel', event => { event.preventDefault(); if (current() && p.api.canLeave()) close(); }, { signal });
  window.addEventListener('keydown', event => {
    if (event.key === 'Escape' && !event.defaultPrevented && current() && p.api.canLeave()) { event.preventDefault(); close(); }
  }, { signal });
  panel.show?.();
  const owner = view(title);
  dismiss.focus({ preventScroll: true });
  queueMicrotask(() => { if (current()) dismiss.scrollIntoView({ block: 'nearest', behavior: 'instant' }); });
  return owner;
}

// Forms share outcome handling; the API remains the authority on uncertainty.
export function formSubmission(p, controls, submit, draft) {
  let busy = false, blocked = false;
  const sync = () => {
    controls.disabled = submit.disabled = busy || blocked;
    controls.setAttribute('aria-busy', String(busy));
    p.syncActivity?.();
  };
  return {
    get blocked() { return blocked; },
    block() { blocked = true; sync(); },
    async run(write, saved, failed) {
      if (busy || blocked || controls.disabled || !p.current() || !p.api.canChange()) return;
      busy = true; sync();
      try {
        const result = await write();
        if (!p.current()) return;
        blocked = true; draft?.clean();
        await saved(result);
      } catch (error) {
        if (!p.current()) return;
        blocked ||= !!error.uncertain || !!error.mutation_applied;
        await failed(error, blocked);
      } finally {
        busy = false;
        if (p.current()) sync();
      }
    },
  };
}

// Once a write succeeds, a failed read must never revive an editable stale form.
export async function readAfterWrite(p, read, notice = 'Changes saved.', tone = '') {
  clearSecrets(p.root);
  p.root.replaceChildren(message(notice, tone), message('Loading current details…'));
  try { await read(notice, tone); }
  catch (error) {
    if (p.current()) p.root.replaceChildren(message(notice, tone), problem(error),
      leaveButton('Reload details', () => readAfterWrite(p, read, notice, tone), p.api));
  }
}
