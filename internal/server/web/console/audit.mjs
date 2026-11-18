import { el, text, leaveButton, actions, card, date, message, problem } from './dom.mjs';

export async function auditHistory(p) {
  const root = card('Access history', text('Committed changes to people, invitations, keys, teams and quotas. This view covers the current audit file, not inference requests or every OAuth sign-in.', 'muted'));
  const feedback = el('div', { 'aria-live': 'polite' });
  const rows = el('tbody');
  const footer = el('div');
  let cursor, count = 0, busy = false;
  const refresh = leaveButton('Refresh', () => read(true), p.api);
  const older = leaveButton('Load older', () => read(false), p.api);
  root.append(actions(refresh), feedback, el('div', { class: 'table-wrap' }, el('table', {},
    el('thead', {}, el('tr', {}, ['When', 'Action', 'Actor', 'Subject', 'Details'].map(label => el('th', { scope: 'col' }, label)))), rows)), footer);
  p.root.append(root);
  async function read(reset) {
    if (busy || !p.current()) return;
    busy = refresh.disabled = older.disabled = true;
    feedback.replaceChildren(message('Reading access history…'));
    try {
      const page = await p.api.data('/access/audit?limit=50' + (!reset && cursor ? '&cursor=' + encodeURIComponent(cursor) : ''), { signal: p.scope.signal });
      if (!p.current()) return;
      if (reset) { rows.replaceChildren(); count = 0; }
      for (const event of page.events) rows.append(el('tr', {},
        el('td', {}, date(event.time)), el('td', {}, event.action), el('td', {}, event.actor), el('td', {}, event.subject),
        el('td', {}, el('details', {}, el('summary', {}, 'Inspect'), event.request_id ? text('Request: ' + event.request_id) : null, el('pre', {}, JSON.stringify(event.metadata || {}, null, 2))))));
      count += page.events.length; cursor = page.next;
      feedback.replaceChildren(...(page.skipped ? [message('Some unreadable records or incomplete fragments were skipped.', 'warning')] : []));
      footer.replaceChildren(text(count ? `${count} event${count === 1 ? '' : 's'} shown.` : 'No recorded changes yet.', 'muted'));
      if (cursor) footer.append(count < 1000 ? older : text('Display limit reached. Refresh for the latest events; older history remains in the local audit file.', 'muted'));
    } catch (error) {
      if (p.current()) {
        feedback.replaceChildren(problem(error));
        if (error.status === 409) feedback.append(message('Refresh to read the current audit file.', 'warning'));
      }
    } finally { if (p.current()) busy = refresh.disabled = older.disabled = false; }
  }
  await read(true);
}
