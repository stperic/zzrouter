import { el, text, leaveButton, actions, card, date, message, problem, field, input } from './dom.mjs';

export async function auditHistory(p) {
  const root = card('Access history', text('Committed changes to people, invitations, keys, teams and quotas. This view covers the coordinator\'s access history, not inference requests or every OAuth sign-in.', 'muted'));
  const feedback = el('div', { 'aria-live': 'polite' });
  const rows = el('tbody');
  const footer = el('div');
  const filters = { actor: input('audit_actor', '', { placeholder: 'Exact actor ID' }), subject: input('audit_subject', '', { placeholder: 'Exact subject ID' }),
    action: input('audit_action', '', { placeholder: 'For example key.created' }), since: input('audit_since', '', { type: 'datetime-local' }), until: input('audit_until', '', { type: 'datetime-local' }) };
  const form = el('form', { class: 'access-form' }, el('div', { class: 'workload-filters' }, field('Actor', filters.actor), field('Subject', filters.subject),
    field('Action', filters.action), field('From', filters.since), field('Before', filters.until)), el('div', { class: 'filter-actions' }, el('button', { type: 'submit' }, 'Apply filters')));
  let cursor, query = '', count = 0, busy = false;
  const refresh = leaveButton('Refresh', () => read(true), p.api);
  const older = leaveButton('Load older', () => read(false), p.api);
  root.append(form, actions(refresh), feedback, el('div', { class: 'table-wrap' }, el('table', {},
    el('thead', {}, el('tr', {}, ['When', 'Action', 'Actor', 'Subject', 'Details'].map(label => el('th', { scope: 'col' }, label)))), rows)), footer);
  p.root.append(root);
  form.addEventListener('submit', event => { event.preventDefault(); void read(true, true); });
  async function read(reset, apply = false) {
    if (busy || !p.current()) return;
    busy = refresh.disabled = older.disabled = true;
    feedback.replaceChildren(message('Reading access history…'));
    try {
      // Only Apply reads the form, from a fresh first page: Refresh and the cursor keep the filters the list shows.
      const wanted = !apply ? query : Object.entries(filters).filter(([, control]) => control.value.trim())
        .map(([name, control]) => `&${name}=${encodeURIComponent(control.type === 'datetime-local' ? new Date(control.value).toISOString() : control.value.trim())}`).join('');
      const page = await p.api.data('/access/audit?limit=50' + wanted + (!reset && cursor ? '&cursor=' + encodeURIComponent(cursor) : ''), { signal: p.scope.signal });
      if (!p.current()) return;
      if (reset) { rows.replaceChildren(); count = 0; query = wanted; }
      for (const event of page.events) rows.append(el('tr', {},
        el('td', {}, date(event.time)), el('td', {}, event.action), el('td', {}, event.actor), el('td', {}, event.subject),
        el('td', {}, el('details', {}, el('summary', {}, 'Inspect'), event.request_id ? text('Request: ' + event.request_id) : null, el('pre', {}, JSON.stringify(event.metadata || {}, null, 2))))));
      count += page.events.length; cursor = page.next;
      feedback.replaceChildren();
      footer.replaceChildren(text(count ? `${count} event${count === 1 ? '' : 's'} shown.` : query ? 'No recorded changes match these filters.' : 'No recorded changes yet.', 'muted'));
      if (cursor) footer.append(count < 1000 ? older : text('Display limit reached. Refresh for the latest events; older events stay recorded on the coordinator.', 'muted'));
    } catch (error) {
      if (p.current()) {
        feedback.replaceChildren(problem(error));
        if (error.status === 409) feedback.append(message('Refresh to read the current history.', 'warning'));
      }
    } finally { if (p.current()) busy = refresh.disabled = older.disabled = false; }
  }
  await read(true);
}
