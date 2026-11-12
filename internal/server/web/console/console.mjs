import { API, Lifetime, Obsolete } from './api.mjs';
import { el, text, button, leaveButton, badge, actions, message, card, heading, field, input, facts, copyField, clearSecrets, problem, date, money } from './dom.mjs';
import { providers } from './providers.mjs';
import { access } from './access.mjs';

const app = document.querySelector('#app');
const pages = new Lifetime();
let expiryTimer;
let refreshVersion = 0;
let route = 'overview';
let section;
let publicState;
let content;
let navigation;
let notices;
let rotation;
let disclosure;
let probeVersion = 0;
let activeProbe;
let recoveryNotice = '';
const verificationWaiters = new Set();
const channel = typeof BroadcastChannel === 'function' ? new BroadcastChannel('zzrouter-console-auth') : null;
const announce = () => channel?.postMessage('auth-state-changed');
const brand = () => el('div', { class: 'brand' }, el('span', { class: 'brand-mark', 'aria-hidden': 'true' }, 'zz'), el('span', {}, 'zzRouter'));

const api = new API({
  cleared(reason) {
    clearTimeout(expiryTimer);
    rotation = null;
    disclosure = null;
    probeVersion++;
    activeProbe = null;
    pages.end();
    clearSecrets(app);
    app.replaceChildren(el('main', { class: 'startup', id: 'content' }, brand(), noticeArea(), text(reason, 'muted')));
    app.setAttribute('aria-busy', 'true');
  },
  rejected() { void refreshSession('Access changed. Review the current state before another change. No request was retried.'); },
  activity: syncActivity,
});

function noticeArea() {
  notices = el('div', { 'aria-live': 'polite' });
  syncActivity();
  return notices;
}

function syncActivity() {
  notices?.replaceChildren(...[
    recoveryNotice ? message(recoveryNotice, 'warning') : null,
    api.unknownChange ? message('A change was interrupted or its response was unreadable. Its outcome may be unknown. Inspect current state before another change. Nothing was retried automatically.', 'warning') : null,
    api.unverified ? message('Browser access could not be verified. New changes are paused; your current form or one-time key is retained.', 'warning') : null,
    api.unverified ? button('Verify session', () => probeSession()) : null,
    api.pending.size ? message('A change is in progress. Wait before leaving this view.') : null,
  ].filter(Boolean));
  for (const control of app.querySelectorAll('[data-leave-view]')) control.disabled = !api.canLeave();
  for (const control of app.querySelectorAll('[data-protected-actions]')) control.inert = !api.canChange();
}

function expiryDeadline(state) {
  return Math.min(...[state?.expires_at, state?.identity?.expires_at]
    .filter(value => typeof value === 'string' && value !== '')
    .map(value => Date.parse(value)).filter(Number.isFinite));
}

function scheduleExpiry(deadline) {
  clearTimeout(expiryTimer);
  if (Number.isFinite(deadline)) expiryTimer = setTimeout(() => { void refreshSession('Your browser access expired.'); }, Math.max(0, Math.min(deadline - Date.now(), 2147483647)));
}

async function probeSession() {
  const anchor = disclosure || api.state;
  if (!anchor) return;
  const generation = api.generation;
  const version = ++probeVersion;
  activeProbe = version;
  try {
    const observed = await api.data('/session', { notifyAuth: false });
    if (version !== probeVersion || generation !== api.generation) return;
    const sameAnchor = observed.issuer === anchor.issuer && observed.csrf === anchor.csrf;
    // Rotation can invalidate the old credential before its response arrives.
    // Only this operation/disclosure may accept that transition on the SAME
    // signed session. Never install a newly observed proof under existing DOM.
    const rotating = rotation?.generation === generation;
    if (sameAnchor && (observed.authenticated === anchor.authenticated || (!observed.authenticated && (rotating || disclosure)))) {
      // The pending write already freezes normal actions during own rotation.
      // A matching unauthenticated probe still authorizes its one-time result.
      api.unverified = false;
      if (rotation) rotation.invalidated = !observed.authenticated;
      syncActivity();
      return;
    }
    await refreshSession('Browser access changed. Review current state before another change.');
  } catch {
    if (version !== probeVersion || generation !== api.generation) return;
    api.unverified = true;
    syncActivity();
  } finally {
    if (activeProbe === version) {
      activeProbe = null;
      for (const wake of verificationWaiters) wake();
    }
  }
}

async function beforeKeyDisclosure(signal, generation) {
  while (true) {
    if (signal.aborted || generation !== api.generation) throw new Obsolete();
    if (!activeProbe && !api.unverified) return;
    await new Promise((resolve, reject) => {
      const cleanup = () => { verificationWaiters.delete(wake); signal.removeEventListener('abort', abort); };
      const wake = () => { cleanup(); resolve(); };
      const abort = () => { cleanup(); reject(new Obsolete()); };
      verificationWaiters.add(wake);
      signal.addEventListener('abort', abort, { once: true });
    });
  }
}

async function refreshSession(notice = '') {
  const version = ++refreshVersion;
  try {
    const state = await api.refresh();
    if (version !== refreshVersion) return;
    publicState = { issuer: state.issuer, node_name: state.node_name };
    app.setAttribute('aria-busy', 'false');
    if (state.mismatch || !state.identity) renderLogin(state, notice);
    else renderShell(state, notice);
  } catch (error) {
    if (version !== refreshVersion || error instanceof Obsolete) return;
    renderRecovery(error);
  }
}

function issuerLink() {
  return publicState?.issuer ? el('a', { href: publicState.issuer + '/console/' }, 'Open the configured coordinator') : null;
}

function renderRecovery(error) {
  pages.end();
  clearSecrets(app);
  app.setAttribute('aria-busy', 'false');
  app.replaceChildren(el('main', { class: 'startup', id: 'content' }, brand(), noticeArea(), el('h1', {}, 'Connection needs attention'), problem(error),
    text(error.status === 421 || error.status === 404 ? 'The console is available on the coordinator with browser authentication enabled. Check the configured public URL.' : 'Protected content has been cleared. Check the coordinator and refresh your session.'), actions(button('Refresh session', () => refreshSession(), 'primary')), issuerLink()));
}

function renderLogin(state, notice) {
  const scope = pages.next();
  const right = el('main', { class: 'login-form', id: 'content' }, el('p', { class: 'eyebrow' }, 'YOUR ROUTER, YOUR ACCESS'), el('h1', {}, 'Sign in to your router'), noticeArea(), text('Use a zzRouter user or administrator key. Your cloud provider keys belong in Providers after sign-in.', 'muted'), facts([['Coordinator', state.node_name], ['Public URL', state.issuer]]));
  if (notice) right.append(message(notice, 'warning'));
  if (state.mismatch) {
    right.append(message('This address does not match the configured browser origin. Open the exact public URL before entering a key.', 'warning'), el('a', { href: state.issuer + '/console/' }, 'Continue at ' + state.issuer));
  } else {
    const key = input('key', '', { type: 'password', required: true, autocomplete: 'off', spellcheck: 'false', maxlength: '8192' });
    const feedback = el('div', { 'aria-live': 'polite' });
    const submit = el('button', { type: 'submit', class: 'primary' }, 'Sign in');
    const form = el('form', { autocomplete: 'off', 'data-protected-actions': true }, field('Router API key', key), submit, feedback);
    form.addEventListener('submit', async event => {
      event.preventDefault();
      if (!form.reportValidity() || submit.disabled || !api.canChange()) return;
      const candidate = key.value;
      key.value = '';
      submit.disabled = true;
      feedback.replaceChildren(message('Signing in…'));
      try {
        await api.data('/session', { method: 'POST', body: { key: candidate }, signal: scope.signal });
        if (!scope.current()) return;
        recoveryNotice = '';
        announce();
        await refreshSession();
      } catch (error) {
        if (!scope.current()) return;
        feedback.replaceChildren(problem(error), leaveButton('Refresh sign-in', () => refreshSession(), api));
        // A rejected proof or unknown login outcome needs an explicit bootstrap.
        submit.disabled = error.uncertain || error.status === 403;
      }
    });
    right.append(form);
  }
  right.append(text('Need access? Ask your router administrator for a named key. Installation recovery is available through the local terminal client.', 'footer-note'));
  app.replaceChildren(el('div', { class: 'login' }, el('aside', { class: 'login-story' }, brand(), el('div', {}, el('p', { class: 'eyebrow' }, 'ONE CONNECTION TO YOUR MODELS'), el('h1', {}, 'Manage your router'), text('Connect your providers, manage access, and bring your router to the tools you use.')), el('small', {}, 'Self-hosted. Connected on your terms.')), right));
}

function renderShell(state, notice) {
  const identity = state.identity;
  const admin = identity.role === 'admin';
  navigation = el('nav', { class: 'nav', 'aria-label': 'Main navigation' });
  for (const [id, label] of admin ? [['overview', 'Overview'], ['providers', 'Providers'], ['access', 'Keys & teams'], ['connections', 'MCP connections'], ['settings', 'Settings']] : [['connections', 'My Access']]) {
    const item = leaveButton(label, () => go(id), api);
    item.dataset.page = id;
    navigation.append(item);
  }
  content = el('main', { id: 'content' });
  const workspace = el('div', { class: 'workspace' }, el('div', { class: 'topbar' }, el('span', {}, el('span', { class: 'connection-dot' }), state.node_name), actions(el('span', {}, identity.name || identity.id), badge(admin ? 'Administrator' : 'User'), leaveButton('Sign out', signOut, api))), noticeArea(), notice ? message(notice, 'warning') : null, content);
  app.replaceChildren(el('div', { class: 'layout' }, el('aside', { class: 'sidebar' }, el('div', {}, brand(), text(state.node_name, 'node-label')), navigation, el('div', { class: 'sidebar-footer' }, el('div', { class: 'identity-name' }, 'Your private AI gateway'), text('Cloud credentials and access, managed in one place.'))), workspace));
  scheduleExpiry(expiryDeadline(state));
  void go(admin ? route : 'connections', section);
}

async function signOut() {
  if (!api.canLeave()) return;
  try {
    await api.logout();
    announce();
    await refreshSession('Signed out. Existing MCP grants remain active until you revoke them.');
  } catch (error) { announce(); renderRecovery(error); }
}

function beginOwnRotation() {
  const anchor = Object.freeze({ issuer: api.state.issuer, csrf: api.state.csrf, authenticated: false, deadline: expiryDeadline(api.state) });
  rotation = { anchor, generation: api.generation, invalidated: false };
  return anchor;
}

function endOwnRotation(anchor) {
  if (rotation?.anchor !== anchor) return;
  const invalidated = rotation.invalidated;
  rotation = null;
  if (invalidated) void recoverOwnRotation();
}

function noteOwnRotationRecovery() {
  recoveryNotice = 'The replacement key was not received. Use another administrator key or local terminal recovery to inspect and rotate the affected key. No write was retried.';
  syncActivity();
}

function recoverOwnRotation() {
  noteOwnRotationRecovery();
  announce();
  return refreshSession();
}

function showKeyAfterSession(result, anchor) {
  api.clear('Your current key was rotated.');
  disclosure = anchor;
  announce();
  app.setAttribute('aria-busy', 'false');
  app.replaceChildren(el('main', { class: 'startup', id: 'content' }, brand(), noticeArea(), el('h1', {}, 'Save your new key'), message('Your old key and browser session no longer work. Save this new value now; it disappears on refresh or when you leave this view.', 'warning'), copyField('New key', result.key, true), button('I saved the key. Sign in', () => refreshSession(), 'primary')));
  scheduleExpiry(anchor.deadline);
}

async function go(name, sub) {
  if (!api.state?.identity || !api.canLeave()) return;
  const allowed = api.state.identity.role === 'admin' ? ['overview', 'providers', 'access', 'connections', 'settings'] : ['connections'];
  route = allowed.includes(name) ? name : allowed[0];
  section = sub;
  clearSecrets(content);
  const scope = pages.next();
  content.replaceChildren();
  for (const item of navigation.children) {
    if (item.dataset.page === route) item.setAttribute('aria-current', 'page');
    else item.removeAttribute('aria-current');
  }
  const generation = api.generation;
  const p = { api, root: content, scope, identity: api.state.identity, state: api.state, current: () => scope.current() && generation === api.generation, go,
    refresh: notice => { announce(); return refreshSession(notice); }, showKeyAfterSession, beginOwnRotation, endOwnRotation, recoverOwnRotation, noteOwnRotationRecovery, syncActivity,
    beforeKeyDisclosure: signal => beforeKeyDisclosure(signal, generation) };
  try {
    if (route === 'providers') await providers(p);
    else if (route === 'access') await access(p, sub === 'teams' ? 'teams' : 'keys');
    else if (route === 'connections') await connections(p);
    else if (route === 'settings') settings(p);
    else await overview(p);
  } catch (error) {
    if (p.current()) content.append(problem(error), leaveButton('Reload page', () => go(route, section), api));
  } finally { syncActivity(); }
}

async function overview(p) {
  p.root.append(heading('Overview', 'Your coordinator, connections and access at a glance.'), message('Reading router status…'));
  const results = await Promise.allSettled([
    api.request('/health', { signal: p.scope.signal }), api.list('/providers/catalog?kind=all', { signal: p.scope.signal }), api.list('/keys', { signal: p.scope.signal }),
  ]);
  if (!p.current()) return;
  const [health, catalog, keys] = results;
  const tiles = el('div', { class: 'cards' });
  tiles.append(health.status === 'fulfilled' ? card('Coordinator', el('div', { class: 'stat' }, 'Online'), text(`Version ${health.value.version}`, 'stat-label'), text('The process is responding. This is not an upstream-provider health check.', 'muted')) : card('Coordinator', problem(health.reason)));
  if (catalog.status === 'fulfilled') {
    const cloud = catalog.value.filter(entry => entry.is_cloud);
    tiles.append(card('Cloud credentials', el('div', { class: 'stat' }, `${cloud.filter(entry => entry.available).length} / ${cloud.length}`), text('Providers with credential configuration', 'stat-label'), leaveButton('Manage providers', () => go('providers'), api, 'link')));
  } else tiles.append(card('Cloud credentials', problem(catalog.reason)));
  tiles.append(keys.status === 'fulfilled' ? card('Managed keys', el('div', { class: 'stat' }, keys.value.length), text('Named keys issued by this router', 'stat-label'), leaveButton('Manage access', () => go('access'), api, 'link')) : card('Managed keys', problem(keys.reason)));
  p.root.replaceChildren(heading('Overview', 'Your coordinator, connections and access at a glance.'), tiles,
    el('h2', { class: 'section-label' }, 'NEXT STEPS'), card('Connect a tool from anywhere', text('Use your configured public URL to connect an MCP client. Give each person or client a named user key and, when needed, a shared team with model limits.'), actions(leaveButton('MCP connection details', () => go('connections'), api, 'primary'), leaveButton('Create access', () => go('access'), api))),
    card('Your installation', facts([['Coordinator', p.state.node_name], ['Public URL', p.state.issuer], ['Signed in as', p.identity.name || p.identity.id]]), leaveButton('Review settings', () => go('settings'), api, 'link')));
}

function limitFacts(limits) {
  return facts([['Requests / minute', limits.rpm_limit || 'No limit at this scope'], ['Tokens / minute', limits.tpm_limit || 'No limit at this scope'], ['Parallel requests', limits.max_parallel_requests || 'No limit at this scope'], ['Spend limit', limits.spend_limit ? money(limits.spend_limit) : 'No limit at this scope'], ['Spend reset', limits.reset_period || 'Default'], ['Output-token reservation', limits.default_max_tokens || 'Server default']]);
}

async function connections(p) {
  const admin = p.identity.role === 'admin';
  p.root.append(heading(admin ? 'MCP connections' : 'My Access', 'Connect your tools and review the access granted to this key.'));
  const endpoints = card('Connect an MCP client', copyField('Inference MCP endpoint', p.state.issuer + '/inference/mcp'),
    text('Add this as a remote HTTP MCP server in your client. Choose browser sign-in, use a zzRouter user key, and approve the named client’s request.', 'muted'));
  if (admin) endpoints.append(message('Administrator keys manage the router. Create a separate user key for inference MCP and model requests.'), copyField('Administrator MCP endpoint', p.state.issuer + '/zzrouter/v1/mcp'), text('Use an administrator key only for tools that should manage the router.', 'muted'));
  const identity = card('Your key', facts([['Name', p.identity.name], ['ID', p.identity.id], ['Role', p.identity.role], ['Source', p.identity.is_virtual ? 'Managed key' : 'Startup key'], ['Expires', date(p.identity.expires_at)]]), limitFacts(p.identity.limits));
  const team = p.identity.team ? card('Your team', facts([['Name', p.identity.team.name], ['Allowed models / groups', p.identity.team.allowed_models.join(', ') || 'All models']]), limitFacts(p.identity.team.limits)) : card('Model access', text('This startup key has no team-specific model restrictions. Its role still determines which APIs it can use.'));
  const usage = p.identity.usage;
  const activity = card('Your recorded usage', usage ? facts([['Spend', money(usage.spend_usd)], ['Reserved', money(usage.reserved_usd)], ['Requests', usage.request_count], ['Tokens in / out', `${usage.tokens_in} / ${usage.tokens_out}`], ['Period start', usage.period_start ? date(usage.period_start) : 'No recorded period']]) : text('Usage is unavailable for this identity. Startup keys do not have a managed-key usage record.', 'muted'));
  const grants = card('Authorized clients', message('Loading your MCP grants…'));
  p.root.append(endpoints, el('div', { class: 'cards' }, identity, team, activity), el('p'), grants, text('Signing out ends browser access. Revoking a client ends that MCP grant and does not sign you out of this console.', 'footer-note'));
  try {
    const entries = await api.list('/me/connections', { signal: p.scope.signal });
    if (!p.current()) return;
    grants.replaceChildren(el('h2', {}, 'Authorized clients'));
    if (!entries.length) grants.append(text('No active MCP grants for this key.', 'empty'));
    for (const grant of entries) {
      const feedback = el('div');
      const revoke = button('Revoke', async () => {
        if (revoke.disabled || !api.canChange() || !confirm(`Revoke ${grant.client}? This client must be authorized again before it can connect.`) || !p.current()) return;
        revoke.disabled = true;
        try {
          await api.data(`/me/connections/${encodeURIComponent(grant.id)}`, { method: 'DELETE', signal: p.scope.signal });
          if (p.current()) { row.remove(); feedback.replaceChildren(message(`${grant.client}: grant revoked.`)); }
        } catch (error) {
          if (!p.current()) return;
          feedback.replaceChildren(problem(error), leaveButton('Reload grants', () => go('connections'), api));
          revoke.disabled = error.uncertain;
        }
      }, 'danger');
      revoke.setAttribute('data-protected-actions', '');
      const row = el('div', { class: 'row' }, el('div', {}, el('h3', {}, grant.client), text(grant.resource), text(`Expires ${date(grant.expires_at)}`)), revoke);
      grants.append(row, feedback);
    }
  } catch (error) { if (p.current()) grants.replaceChildren(el('h2', {}, 'Authorized clients'), problem(error), leaveButton('Reload grants', () => go('connections'), api)); }
}

function settings(p) {
  p.root.append(heading('Settings', 'Review effective browser configuration and installation controls.'),
    card('Browser authentication', facts([['Public URL / OAuth issuer', p.state.issuer], ['Coordinator', p.state.node_name], ['Session expires', date(p.state.expires_at)], ['Authentication', 'Named zzRouter keys and HttpOnly browser sessions']]), text('This public URL is fixed at startup. It must exactly match the address opened in your browser, including scheme, hostname and port.'), message('To change it, update auth.oauth.issuer_url in the coordinator installation and restart. Update your MCP clients to the same URL.')),
    card('Remote access', text('Use a stable HTTPS public URL for home or office access. A tunnel or Cloudflare Access can protect the outer connection; zzRouter still verifies the key, session and MCP authorization.'), text('Your proxy must allow the OAuth discovery and authorization flow required by your MCP clients. Never treat a forwarded header as a router identity.', 'muted')),
    card('Protected provider credentials', text('Configure security.credentials.key_file locally before saving cloud or Hugging Face keys. The protected store is node-local; provider secrets are never copied to workers.'), text('Keep the protection key outside provider configuration and back it up securely with the encrypted state. Listener, issuer, TLS and protection-key changes require local installation access and a restart.', 'muted'), actions(leaveButton('Manage provider keys', () => go('providers'), api, 'primary'))),
    card('Local recovery', text('Use the terminal client for provider installation, local runtimes, models, jobs and installation recovery. The web and terminal clients share the coordinator APIs.'), el('a', { href: '/openapi.yaml' }, 'View the API contract')));
}

channel?.addEventListener('message', event => { if (event.data === 'auth-state-changed') void refreshSession('Browser access changed in another tab.'); });
window.addEventListener('focus', event => { if (event.target === window) void probeSession(); });
window.addEventListener('pageshow', event => { if (event.persisted) void (api.state || disclosure ? probeSession() : refreshSession()); });
window.addEventListener('beforeunload', event => { if (!api.canLeave()) { event.preventDefault(); event.returnValue = ''; } });
window.addEventListener('pagehide', () => { api.clear('Session paused.'); });
void refreshSession();
