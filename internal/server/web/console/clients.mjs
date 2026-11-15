import { el, text, leaveButton, badge, actions, heading, facts, message, problem, date, copyField, card, markSelection } from './dom.mjs';
import { clientKey, clientLabel, clientState, protocolName } from './network-data.mjs';
import { details } from './details.mjs';

export const readClients = p => Promise.allSettled([
  p.api.request('/zzrouter/v1/clients', { signal: p.scope.signal }),
  p.api.list('/me/connections', { signal: p.scope.signal }),
]);

export function clientTile(p, row, page = 'clients') {
  const tile = leaveButton('', () => p.go(page, `client/${clientKey(row)}`), p.api, 'network-item');
  tile.dataset.client = clientKey(row);
  tile.setAttribute('data-selection', `client/${clientKey(row)}`);
  tile.append(el('span', { class: 'network-item-title' }, el('strong', {}, clientLabel(row)), badge(...clientState(row))),
    el('span', { class: 'network-meta' }, protocolName(row.protocol)),
    el('span', { class: 'network-hardware' }, row.active_requests > 0 ? `${row.active_requests} request${row.active_requests === 1 ? '' : 's'} at last refresh` : date(row.last_seen_at)));
  return tile;
}

export function showClient(p, row) {
  if (!row) { p.root.append(message('This client activity is no longer in the observation window.', 'warning')); return; }
  p.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, clientLabel(row)), row.source === 'managed' ? leaveButton('View key', () => p.go('access', `keys/${row.key_id}`), p.api) : null), badge(...clientState(row)),
    facts([['Protocol', protocolName(row.protocol)], ['Key ID', row.key_id], ['Key source', row.source === 'managed' ? 'Managed key' : 'Startup key'],
      ['Authentication', row.authentication === 'oauth' ? 'OAuth' : 'API key'], ['Requests at refresh', row.active_requests],
      ['Last activity', date(row.last_seen_at)], ['First observed', date(row.first_seen_at)], ['Observed requests', row.request_count]]),
    text('The label shows the key name recorded at the last activity. Activity is grouped by key identity, protocol and authentication method. Multiple applications may share one identity. This is not a device count or a current authorization check.', 'muted'));
}

export function connectionGuide(p) {
  const guide = card('Connect a client', text('Give each application its own named user key so its activity is easy to recognize.', 'muted'),
    el('section', { class: 'access-section' }, el('h3', {}, '1. Create a client key'),
      text('Choose a team with the routes, providers and budget this client should use.'),
      p.identity.role === 'admin' ? leaveButton('+ Create key', () => p.go('access', 'keys', 'create'), p.api, 'primary') : text('Use your current user key, or ask your administrator for one.')),
    el('section', { class: 'access-section' }, el('h3', {}, '2. Connect your application'),
      copyField('Inference MCP endpoint', p.state.issuer + '/inference/mcp'), text('Add a remote HTTP MCP server in your application. Choose browser sign-in, use the client’s user key, and approve the named client.', 'muted'),
      copyField('OpenAI-compatible API base URL', p.state.issuer + '/v1'), text('For an API client, use this base URL and your client key. Select a route or model allowed by its team.', 'muted')),
    el('section', { class: 'access-section' }, el('h3', {}, '3. Send a request'), text('The client appears in the overview after its first authenticated request. Refresh to see its key name and activity.')));
  return guide;
}

export async function clients(p, selection) {
  let rows = [], drawer;
  const listing = el('div', { class: 'entity-list' });
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  const selectClient = next => {
    drawer?.dispose(!next); drawer = undefined; selection = next;
    markSelection(listing, next);
    if (!next) return;
    drawer = details({ ...p, root: workspace }, next === 'connect' ? 'Connect client' : 'Client details', () => p.go('clients'));
    if (next === 'connect') drawer.root.append(connectionGuide(drawer));
    else showClient(drawer, rows.find(row => clientKey(row) === next.slice(7)));
  };
  const read = async () => {
    const [activity, grants] = await readClients(p);
    if (!p.current()) return;
    if (activity.status === 'rejected') throw activity.reason;
    const result = activity.value; rows = result.data || [];
    listing.replaceChildren(text(`Updated ${date(new Date().toISOString())}.`, 'network-caption'),
      el('div', { class: 'node-grid' }, rows.map(row => clientTile(p, row))),
      text('Activity is reported at refresh. A client can remain connected between requests.', 'muted'));
    if (!rows.length) listing.append(message('No client activity yet.'), leaveButton('Connect a client', () => p.go('clients', 'connect'), p.api));
    if (result.metadata?.truncated) listing.append(message('The observation limit was reached. Some activity is omitted.', 'warning'));
    listing.append(text(`Observed since ${date(result.metadata?.observed_since)}. Inactive entries expire after 24 hours; observations reset when the coordinator restarts.`, 'footer-note'),
      actions(leaveButton('Manage keys & teams', () => p.go('access'), p.api), leaveButton(grants.status === 'fulfilled' ? `Your authorizations (${grants.value.length})` : 'Your authorizations', () => p.go('connections'), p.api)));
    markSelection(listing, selection);
  };
  const refresh = async () => {
    try { await read(); if (p.current()) selectClient(selection); }
    catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  p.root.append(heading('Clients', 'Client activity labeled by key name and grouped by protocol.',
    leaveButton('Refresh', refresh, p.api), leaveButton('+ Connect client', () => p.go('clients', 'connect'), p.api, 'primary')), workspace);
  listing.append(message('Reading client activity…'));
  await read();
  if (p.current()) { p.handleSelection?.(selectClient); selectClient(selection); }
}
