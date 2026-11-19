import { el, text, badge, leaveButton } from './dom.mjs';

// This is presentation of the session API's configuration assessment, not a
// second URL classification or a claim that another machine can connect.
export function sharingReadiness(p, { settings = false } = {}) {
  const status = p.state.sharing;
  const local = status === 'local_only';
  return el('section', { class: 'access-section' }, el('h3', {}, 'Sharing this router'),
    badge(local ? 'This computer only' : status === 'configured' ? 'HTTPS address configured' : 'Sharing status unavailable', local ? 'warn' : ''),
    text(local ? 'Links use a local address. They work on this computer; another person or a remote app needs a reachable HTTPS address.'
      : status === 'configured' ? 'Before sending a link, open the console address from the other device. Address configuration does not verify its network access.'
        : 'Reload your session to check the configured address before sharing links.', 'muted'),
    settings ? el('details', { class: 'network-caption' }, el('summary', {}, 'Change the public address'),
      text('Set auth.oauth.issuer_url to a reachable HTTPS address and restart the coordinator. Use that address in browsers and MCP clients. Your proxy must allow OAuth discovery and authorization.', 'muted'))
      : p.identity.role === 'admin' ? leaveButton('Review sharing setup', () => p.go('settings', 'general'), p.api, 'link') : null);
}
