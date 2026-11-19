import { el, text, leaveButton, badge, actions, heading, facts, message, problem, date, copyField, card, markSelection } from './dom.mjs';
import { clientKey, clientLabel, clientState, protocolName } from './network-data.mjs';
import { details } from './details.mjs';
import { sharingReadiness } from './sharing.mjs';

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
    el('span', { class: 'network-hardware' }, row.active_requests > 0 ? `${row.active_requests} active at refresh` : date(row.last_seen_at, 'Last activity unknown')));
  return tile;
}

export function showClient(p, row) {
  if (!row) { p.root.append(message('This client activity is no longer in the observation window.', 'warning')); return; }
  const person = row.source === 'person';
  p.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, clientLabel(row)), person ? leaveButton('View person', () => p.go('people', row.key_id), p.api) : row.source === 'managed' ? leaveButton('View key', () => p.go('access', `keys/${row.key_id}`), p.api) : null), badge(...clientState(row)),
    facts([['Protocol', protocolName(row.protocol)], [person ? 'Person ID' : 'Key ID', row.key_id], ['Identity source', person ? 'Local account' : row.source === 'managed' ? 'Managed key' : 'Startup key'],
      ['Authentication', row.authentication === 'oauth' ? 'OAuth' : 'API key'], ['Requests at refresh', row.active_requests],
      ['Last activity', date(row.last_seen_at)], ['First observed', date(row.first_seen_at)], ['Observed requests', row.request_count]]),
    text('The label shows the account or key name recorded at the last activity. Activity is grouped by identity, protocol and authentication method. Multiple applications may share one identity. This is not a device count or a current authorization check.', 'muted'));
}

export function connectionGuide(p) {
  return card('Connection options', text('Choose the sign-in method your application supports.', 'muted'), sharingReadiness(p),
    el('section', { class: 'access-section' }, el('h3', {}, 'Browser sign-in for an MCP client'),
      text('Add a remote HTTP MCP server, then choose browser sign-in. Use a User account for inference, and approve the named application. Its access follows your team.'),
      copyField('Inference MCP endpoint', p.state.issuer + '/inference/mcp'),
      p.identity.role === 'admin' ? el('div', {}, text('Your Administrator account authorizes management MCP. For model requests, use a User account or a named user app key.', 'muted'), copyField('Management MCP endpoint', p.state.issuer + '/zzrouter/v1/mcp')) : null,
      text('No app key is needed. Enter your account password only on your router’s sign-in page.', 'muted')),
    el('section', { class: 'access-section' }, el('h3', {}, 'API key for an app or automation'),
      text('Create one named user key per application, assigned to a team with the intended routes, providers and budget.'),
      p.identity.role === 'admin' ? leaveButton('+ Create app key', () => p.go('access', 'keys', 'create'), p.api, 'primary') : text('Ask your administrator for a named app key assigned to your team.'),
      copyField('OpenAI-compatible API base URL', p.state.issuer + '/v1'),
      text('Paste the app key into the client’s API key field and select an allowed route or model. Your account password is not an API key.', 'muted')),
    el('section', { class: 'access-section' }, el('h3', {}, 'Verify the connection'),
      text(p.identity.role === 'admin' ? 'Send one request, then refresh Clients. You will see the account or app-key name and its observed activity.' : 'Send one request and check the response in your application. Your administrator can confirm its account name and activity in Clients.')));
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
      rows.length ? el('div', { class: 'node-grid' }, rows.map(row => clientTile(p, row))) : message('No client activity yet. Connect a client and send a request to see it here.'));
    if (result.metadata?.truncated) listing.append(message('The observation limit was reached. Some activity is omitted.', 'warning'));
    listing.append(el('details', { class: 'network-caption' }, el('summary', {}, 'About client activity'),
      result.metadata?.observed_since ? text(`Observed since ${date(result.metadata.observed_since)}.`) : null,
      text('Activity is captured at refresh. Inactive entries expire after 24 hours and the coordinator clears observations on restart.')),
      actions(leaveButton('Manage keys & teams', () => p.go('access'), p.api), leaveButton(grants.status === 'fulfilled' ? `Your authorizations (${grants.value.length})` : 'Your authorizations', () => p.go('connections'), p.api)));
    markSelection(listing, selection);
  };
  const refresh = async () => {
    try { await read(); if (p.current()) selectClient(selection); }
    catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  p.root.append(heading('Clients', 'Client activity labeled by account or app-key name and grouped by protocol.',
    leaveButton('Refresh', refresh, p.api), leaveButton('+ Connect client', () => p.go('clients', 'connect'), p.api, 'primary')), workspace);
  listing.append(message('Reading client activity…'));
  await read();
  if (p.current()) { p.handleSelection?.(selectClient); selectClient(selection); }
}
