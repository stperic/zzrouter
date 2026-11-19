import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, actions, heading, field, input, select, facts, message, problem, date, money, selectableRow, markSelection, compactTable } from './dom.mjs';
import { details } from './details.mjs';
import { eventJSON, readEvents } from './event-stream.mjs';

export function logSummary(entry) {
  // Keep list/live buffers free of the optional inline prompts and responses.
  return Object.fromEntries(['id', 'timestamp', 'model', 'provider', 'node', 'status', 'latency_ms', 'key_id', 'team_id']
    .filter(key => entry[key] !== undefined).map(key => [key, typeof entry[key] === 'string' ? entry[key].slice(0, 2048) : entry[key]]));
}

export function logMatches(entry, filters, now = Date.now()) {
  const duration = { '5m': 300000, '1h': 3600000, '24h': 86400000 }[filters.since];
  const since = duration ? now - duration : filters.since ? Date.parse(filters.since) : undefined;
  return (!filters.model || String(entry.model || '').toLowerCase() === filters.model.toLowerCase()) &&
    (!filters.status || entry.status === filters.status) && (!filters.key_id || entry.key_id === filters.key_id) &&
    (!filters.team_id || entry.team_id === filters.team_id) && (since === undefined || Date.parse(entry.timestamp) >= since);
}

export async function logs(p, selected) {
  const reads = new Lifetime(), streamReads = new Lifetime();
  let records = [], offset = 0, hasMore = false, drawer, filters = {}, selectionVersion = 0;
  const listing = el('section', { class: 'entity-list card' }), results = el('div'), feedback = el('div', { 'aria-live': 'polite' });
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  const model = input('log_model', '', { placeholder: 'Exact model name' });
  const status = select('log_status', [['', 'All outcomes'], ['success', 'Success'], ['error', 'Error']], '');
  const since = select('log_since', [['', 'Retained history'], ['5m', 'Last 5 minutes'], ['1h', 'Last hour'], ['24h', 'Last day']], '');
  const key = input('log_key'), team = input('log_team');
  const form = el('form', { class: 'access-form' }, el('div', { class: 'workload-filters' }, field('Model', model), field('Outcome', status), field('Time range', since)),
    el('div', { class: 'filter-actions' }, el('details', {}, el('summary', {}, 'Key and team filters'),
      el('div', { class: 'form-grid' }, field('Key ID', key), field('Team ID', team))), el('button', { type: 'submit' }, 'Apply filters')));
  let following = false;
  const live = button('Follow live', () => {
    if (!following) return follow();
    streamReads.end(); setFollowing(false);
    feedback.replaceChildren(message('Live updates paused.'));
  });
  function setFollowing(value) {
    following = value;
    live.textContent = following ? 'Pause live' : 'Follow live';
  }
  const older = button('Load older', () => load(true));
  p.root.append(heading('Logs', 'Inspect inference requests. Open a workload for its runtime output.',
    leaveButton('Refresh', () => load(), p.api), live), workspace);
  listing.append(form, feedback, results, older);
  p.scope.signal.addEventListener('abort', () => { reads.end(); streamReads.end(); records = []; }, { once: true });
  async function choose(id) {
    const version = ++selectionVersion;
    drawer?.dispose(!id); drawer = undefined; selected = id; markSelection(results, id);
    if (!id) { render(); return; }
    const owner = details({ ...p, root: workspace }, 'Request details', () => p.go('logs'));
    drawer = owner; owner.root.append(message('Reading request details…'));
    try {
      let entry = await p.api.data(`/inference-logs/${encodeURIComponent(id)}`, { signal: owner.scope.signal });
      if (!owner.current() || version !== selectionVersion) return;
      owner.scope.signal.addEventListener('abort', () => { entry = undefined; }, { once: true });
      const payload = el('div'), reveal = button('Show retained payload', async () => {
        if (reveal.getAttribute('aria-expanded') === 'true') { payload.replaceChildren(); reveal.setAttribute('aria-expanded', 'false'); reveal.textContent = 'Show retained payload'; return; }
        reveal.disabled = true;
        try {
          const value = await p.api.data(`/inference-logs/${encodeURIComponent(id)}/payload`, { signal: owner.scope.signal });
          if (!owner.current()) return;
          payload.replaceChildren(message('Payloads can contain prompts and private information. They are cleared when this panel closes.'),
            ...[['Request', value.request], ['Upstream request', value.upstream_request], ['Response', value.response]].filter(([, data]) => data !== undefined).map(([title, data]) =>
              el('section', {}, el('h3', {}, title), el('pre', { class: 'log-output', tabindex: '0', 'data-sensitive': true }, JSON.stringify(data, null, 2).slice(0, 262144)))),
            value.elided?.length ? text(`${value.elided.length} media payloads were elided by the server.`, 'muted') : '',
            value.oversize ? message('The server omitted oversized payload content.', 'warning') : '');
          reveal.setAttribute('aria-expanded', 'true'); reveal.textContent = 'Hide payload';
        } catch (error) { if (owner.current()) payload.replaceChildren(problem(error)); }
        finally { if (owner.current()) reveal.disabled = false; }
      });
      reveal.setAttribute('aria-expanded', 'false');
      owner.root.replaceChildren(el('h2', {}, entry.model), facts([
        ['Request ID', entry.id], ['Time', date(entry.timestamp, 'Not reported')], ['Outcome', entry.status], ['Provider', entry.provider], ['Node', entry.node],
        ['API', entry.request_type], ['Routing', entry.routing_decision], ['Input tokens', entry.tokens_in], ['Output tokens', entry.tokens_out],
        ['First token', entry.ttft_ms === undefined ? 'Not reported' : `${entry.ttft_ms} ms`], ['Duration', entry.latency_ms === undefined ? 'Not reported' : `${entry.latency_ms} ms`],
        ['Reported cost', entry.cost === undefined ? 'Not reported' : money(entry.cost)], ['Key ID', entry.key_id], ['Team ID', entry.team_id],
      ]), entry.error_message ? message(entry.error_message, 'error') : '',
      entry.payload_available ? reveal : text('The full payload is not retained for this request.', 'muted'), payload);
    } catch (error) { if (owner.current() && version === selectionVersion) owner.root.replaceChildren(problem(error)); }
  }
  function render() {
    const body = el('tbody');
    for (const row of records) {
      const open = () => p.go('logs', row.id);
      body.append(selectableRow(row.id, selected, open, el('td', {}, leaveButton(row.model || 'Unknown model', open, p.api, 'link entity-title'), text(date(row.timestamp, 'Time unknown'), 'muted')),
        el('td', {}, row.provider || 'Unknown', text(row.node || 'Unknown node', 'muted')), el('td', {}, row.status),
        el('td', {}, row.latency_ms === undefined ? 'Not reported' : `${row.latency_ms} ms`)));
    }
    results.replaceChildren(records.length ? compactTable(['Request', 'Destination', 'Outcome', 'Duration'], body) : text('No retained requests match these filters.', 'empty'));
    older.hidden = !hasMore; older.disabled = !hasMore || records.length >= 1000;
  }
  async function load(more = false) {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    streamReads.end(); setFollowing(false);
    const scope = reads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    feedback.replaceChildren(message('Reading requests…')); older.disabled = true;
    const query = new URLSearchParams({ ...filters, limit: '50', offset: String(more ? offset : 0) });
    try {
      const result = await p.api.request('/zzrouter/v1/inference-logs?' + query, { signal });
      if (!p.current() || !scope.current()) return;
      if (!Array.isArray(result.data)) throw new Error('The coordinator returned an invalid request list.');
      records = [...new Map([...(more ? records : []), ...result.data.map(logSummary)].map(entry => [entry.id, entry])).values()].slice(0, 1000);
      offset = (more ? offset : 0) + result.data.length; hasMore = Boolean(result.has_more && result.data.length);
      render(); feedback.replaceChildren(text(`${records.length} ${records.length === 1 ? 'request' : 'requests'} shown.${hasMore ? ' Older pages can shift as new requests arrive.' : ''}`, 'muted'));
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error)); }
  }
  async function follow() {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    reads.end(); const scope = streamReads.next(), signal = AbortSignal.any([p.scope.signal, scope.signal]);
    setFollowing(true); let epoch, sequence = 0, dropped = false;
    feedback.replaceChildren(message('Connecting to live requests…'));
    try {
      const pointer = await p.api.data('/inference-logs/stream', { signal });
      if (!p.current() || !scope.current()) return;
      if (!pointer.job_id) throw new Error('The coordinator did not identify its inference log stream.');
      const response = await p.api.stream(`/jobs/${encodeURIComponent(pointer.job_id)}/stream`, { signal });
      feedback.replaceChildren(text('Live requests. Up to 1,000 entries are retained in this view.', 'muted'));
      await readEvents(response, event => {
        if (!p.current() || !scope.current()) return false;
        if (event.event === 'error') throw new Error(event.data.slice(0, 2048));
        if (event.event === 'stream_closed') return false;
        if (!['progress', 'done', 'events_dropped'].includes(event.event)) return;
        const value = eventJSON(event);
        if (epoch && value.epoch && epoch !== value.epoch) { records = []; sequence = 0; dropped = true; }
        epoch = value.epoch || epoch;
        if (event.event === 'events_dropped') dropped = true;
        if (dropped) feedback.replaceChildren(message('Some live observations were dropped or the stream restarted. Refresh for the retained request history.', 'warning'));
        if (value.seq && value.seq <= sequence) return;
        sequence = value.seq || sequence;
        const row = value.meta?.entry;
        if (row?.id && logMatches(row, filters)) {
          records = [logSummary(row), ...records.filter(entry => entry.id !== row.id)].slice(0, 1000);
          // Preserve focused rows and selected details while live observations arrive.
          if (!results.contains(document.activeElement) && !drawer) render();
        }
        return event.event !== 'done';
      }, signal);
      if (p.current() && scope.current()) feedback.append(message('Live feed ended. Reconnect explicitly to continue.'));
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error)); }
    finally { if (p.current() && scope.current()) setFollowing(false); }
  }
  form.addEventListener('submit', event => {
    event.preventDefault(); filters = Object.fromEntries(Object.entries({ model: model.value.trim(), status: status.value, since: since.value, key_id: key.value.trim(), team_id: team.value.trim() }).filter(([, value]) => value)); void load();
  });
  p.handleSelection?.(choose);
  await load(); if (p.current()) await choose(selected);
}
