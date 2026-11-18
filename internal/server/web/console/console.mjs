import { policySummary } from './access-policy.mjs';
import { usageDetails } from './access-budget.mjs';
import { API, Lifetime, Obsolete } from './api.mjs';
import { el, text, button, leaveButton, badge, actions, message, card, heading, field, input, facts, copyField, clearSecrets, problem, date, money } from './dom.mjs';
import { providers } from './providers.mjs';
import { overview } from './overview.mjs';
import { nodes } from './nodes.mjs';
import { clients, connectionGuide } from './clients.mjs';
import { access } from './access.mjs';
import { routes } from './routes.mjs';
import { workloads } from './workloads.mjs';
import { models } from './models.mjs';
import { deployments } from './deployments.mjs';
import { logs } from './logs.mjs';
import { system } from './system.mjs';
import { initializeAppearance, themePicker } from './appearance.mjs';
import { signIn, takeAccessLink, takeAuthorizationLink } from './login.mjs';
import { people, accountSettings } from './people.mjs';

let authorizationRequest = takeAuthorizationLink(location, window.history);
let accessLink = takeAccessLink(location, window.history);
initializeAppearance();
const app = document.querySelector('#app');
const pages = new Lifetime();
let expiryTimer;
let refreshVersion = 0;
const initialRoute = readRoute();
let route = initialRoute.name;
let section = initialRoute.item;
let mode = initialRoute.mode;
let computeNode = initialRoute.node;
let computeProvider = initialRoute.provider;
let leaveGuard;
let historyIndex = window.history?.state?.zzrouterConsoleIndex || 0;
let historyReady = false;
let restoringHistory = false;
let publicState;
let content;
let pageSelection;
let navigation;
let navigationToggle;
let notices;
let rotation;
let disclosure;
let chatSession;
let probeVersion = 0;
let activeProbe;
let recoveryNotice = '';
const verificationWaiters = new Set();
const channel = typeof BroadcastChannel === 'function' ? new BroadcastChannel('zzrouter-console-auth') : null;
const announce = () => channel?.postMessage('auth-state-changed');
const brand = () => el('div', { class: 'brand', role: 'img', 'aria-label': 'zzRouter' }, el('span', { class: 'brand-mark', 'aria-hidden': 'true' }), el('span', { class: 'brand-wordmark', 'aria-hidden': 'true' }));

const api = new API({
  cleared(reason) {
    chatSession = undefined;
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
  rejected(error) {
    if (error.status === 403) void probeSession();
    else void refreshSession('Access changed. Review the current state before another change. No request was retried.');
  },
  activity: syncActivity,
});

function noticeArea() {
  notices = el('div', { 'aria-live': 'polite', role: 'region', 'aria-label': 'Session status' });
  syncActivity();
  return notices;
}

function syncActivity() {
  chatSession?.view?.enable(api.canChange());
  notices?.replaceChildren(...[
    recoveryNotice ? message(recoveryNotice, 'warning') : null,
    api.unknownChange ? message('A change was interrupted or its response was unreadable. Its outcome may be unknown. Inspect current state before another change. Nothing was retried automatically.', 'warning') : null,
    api.unverified ? el('div', { class: 'notice warning session-verification', role: 'status' },
      el('span', { title: 'Verify your browser access to resume. Your current work is retained.' }, 'New changes are paused'), button('Verify session', () => probeSession())) : null,
    api.pending.size ? message('A change is in progress. Wait before leaving this view.') : null,
  ].filter(Boolean));
  notices?.setAttribute('tabindex', notices.children.length ? '0' : '-1');
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
    if (state.mismatch || !state.identity || accessLink) renderLogin(state, notice);
    else if (authorizationRequest) {
      const destination = authorizationRequest;
      authorizationRequest = null;
      // GET validates the request again and shows fresh, explicit consent.
      window.location.assign(destination);
    } else renderShell(state, notice);
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
  const panel = el('main', { class: 'login-form', id: 'content' }, brand(), noticeArea());
  if (notice) panel.append(message(notice, 'warning'));
  if (authorizationRequest) panel.append(text('After sign-in, you’ll review the application’s request before allowing access.', 'muted'));
  app.replaceChildren(el('div', { class: 'login' }, panel));
  const incoming = accessLink; accessLink = null;
  const current = () => scope.current();
  void signIn({ api, state, root: panel, scope, current, syncActivity,
    refresh: () => refreshSession(), signedIn: () => { recoveryNotice = ''; announce(); return refreshSession(); } }, incoming);
}

function renderShell(state, notice) {
  const identity = state.identity;
  const admin = identity.role === 'admin';
  navigation = el('nav', { id: 'main-navigation', class: 'nav', 'aria-label': 'Main navigation' });
  navigationToggle = button('Navigation', () => setNavigation(navigationToggle.getAttribute('aria-expanded') !== 'true'), 'navigation-toggle');
  navigationToggle.setAttribute('aria-controls', 'main-navigation');
  navigationToggle.setAttribute('aria-expanded', 'false');
  window.addEventListener('keydown', event => {
    if (event.key === 'Escape' && navigationToggle.getAttribute('aria-expanded') === 'true') {
      event.preventDefault(); event.stopImmediatePropagation(); setNavigation(false); navigationToggle.focus();
    }
  }, { signal: api.authority.signal });
  for (const [id, label] of admin ? [['overview', 'Overview'], ['chat', 'Chat'], ['models', 'Models'], ['nodes', 'Nodes'], ['workloads', 'Workloads'], ['logs', 'Logs'], ['clients', 'Clients'], ['providers', 'Providers'], ['routes', 'Routes'], ['access', 'Keys & teams'], ['people', 'People'], ['connections', 'My account'], ['settings', 'System']] : [['chat', 'Chat'], ['connections', 'My Access']]) {
    const item = leaveButton(label, () => go(id), api);
    item.dataset.page = id;
    navigation.append(item);
  }
  navigation.append(themePicker(api.authority.signal));
  content = el('main', { id: 'content', class: 'page-content' });
  const workspace = el('div', { class: 'workspace' }, el('div', { class: 'topbar' }, el('span', {}, 'Coordinator: ' + state.node_name), actions(el('span', {}, identity.name || identity.id), badge(admin ? 'Administrator' : 'User'), leaveButton('Sign out', signOut, api))), noticeArea(), notice ? message(notice, 'warning') : null, content);
  app.replaceChildren(el('div', { class: 'layout' }, el('aside', { class: 'sidebar' }, el('div', { class: 'sidebar-heading' }, brand(), navigationToggle), navigation), workspace));
  scheduleExpiry(expiryDeadline(state));
  void go(admin || ['chat', 'connections'].includes(route) ? route : 'chat', section, mode, { node: computeNode, provider: computeProvider });
}

function setNavigation(open) {
  navigationToggle?.setAttribute('aria-expanded', String(open));
  navigation?.setAttribute('data-open', String(open));
}

function canLeave() { return api.canLeave() && (!leaveGuard || leaveGuard.confirm()); }

async function signOut() {
  if (!canLeave()) return;
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

async function go(name, sub, nextMode, { force = false, checked = false, node: targetNode, provider: targetProvider } = {}) {
  if (!api.state?.identity || !(checked ? api.canLeave() : canLeave())) return;
  setNavigation(false);
  const allowed = api.state.identity.role === 'admin' ? ['overview', 'chat', 'models', 'nodes', 'workloads', 'logs', 'clients', 'providers', 'routes', 'access', 'people', 'connections', 'settings'] : ['chat', 'connections'];
  const destination = allowed.includes(name) ? name : allowed[0];
  const nextNode = ['providers', 'models', 'workloads', 'chat'].includes(destination) ? targetNode : undefined;
  const nextProvider = nextNode ? targetProvider : undefined;
  if (!force && destination === route && nextProvider === computeProvider && nextNode === computeNode && pageSelection?.current() && pageSelection.accepts(sub, nextMode)) {
    section = sub; mode = nextMode;
    writeRoute();
    await pageSelection.select(sub, nextMode);
    syncActivity();
    return;
  }
  pageSelection = undefined;
  leaveGuard = undefined;
  route = destination;
  computeNode = nextNode;
  computeProvider = nextProvider;
  section = sub; mode = nextMode;
  writeRoute();
  clearSecrets(content);
  content.classList.remove('has-details');
  const scope = pages.next();
  content.replaceChildren();
  for (const item of navigation.children) {
    if (item.dataset.page === route) item.setAttribute('aria-current', 'page');
    else item.removeAttribute('aria-current');
  }
  const generation = api.generation;
  const focusBefore = document.activeElement;
  const p = { api, root: content, scope, identity: api.state.identity, state: api.state, computeNode, computeProvider, current: () => scope.current() && generation === api.generation,
    go: (name, sub, mode, node = computeNode, provider = computeProvider) => go(name, sub, mode, { node, provider }),
    handleSelection: (select, accepts = () => true) => { if (scope.current()) pageSelection = { current: () => scope.current() && generation === api.generation, select, accepts }; },
    reload: () => go(route, section, mode, { force: true, node: computeNode, provider: computeProvider }), canLeave,
    guardLeave: (confirm, dirty) => { const guard = { confirm, dirty }; leaveGuard = guard; return () => { if (leaveGuard === guard) leaveGuard = undefined; }; },
    refresh: notice => { announce(); return refreshSession(notice); }, showKeyAfterSession, beginOwnRotation, endOwnRotation, recoverOwnRotation, noteOwnRotationRecovery, syncActivity,
    selectSection: (item, nextMode) => { if (scope.current()) { section = item; mode = nextMode; writeRoute(true); } },
    beforeKeyDisclosure: signal => beforeKeyDisclosure(signal, generation) };
  if (computeNode) content.append(el('div', { class: 'notice compute-context' },
    text(`Preparing models on ${computeNode}`), actions(
      leaveButton('View compute node', () => go('nodes', `node/${computeNode}`), api),
      leaveButton('Show all computers', () => go(route, undefined, route === 'chat' ? undefined : mode), api))));
  try {
    if (route === 'chat') {
      const { chat, createChatSession } = await import('./chat.mjs');
      if (p.current()) { chatSession ||= createChatSession(api); await chat(p, chatSession, sub, nextMode || computeNode); }
    }
    else if (route === 'providers') { const [category, ...key] = (sub || 'local').split('/'); await providers(p, category, key.join('/'), nextMode); }
    else if (route === 'models') await models(p, sub, nextMode);
    else if (route === 'nodes') await nodes(p, sub);
    else if (route === 'workloads') { if (nextMode === 'downloads') await deployments(p, sub); else await workloads(p, sub, nextMode); }
    else if (route === 'logs') await logs(p, sub);
    else if (route === 'clients') await clients(p, sub);
    else if (route === 'routes') await routes(p, sub, nextMode);
    else if (route === 'access') await access(p, sub || 'keys', nextMode);
    else if (route === 'people') await people(p, sub, nextMode);
    else if (route === 'connections') await connections(p);
    else if (route === 'settings') await system(p, sub || 'general', nextMode);
    else await overview(p, sub);
  } catch (error) {
    if (p.current()) content.append(problem(error), leaveButton('Reload page', () => p.reload(), api));
  } finally {
    syncActivity();
    if (p.current() && document.activeElement === focusBefore && !content.querySelector?.('.detail-panel')) content.querySelector?.('h1')?.focus({ preventScroll: true });
  }
}

function limitFacts(limits) {
  return facts([['Requests / minute', limits.rpm_limit || 'No limit at this scope'], ['Tokens / minute', limits.tpm_limit || 'No limit at this scope'], ['Parallel requests', limits.max_parallel_requests || 'No limit at this scope'], ['Spend limit', limits.spend_limit ? money(limits.spend_limit) : 'No limit at this scope'], ['Spend reset', limits.reset_period || 'Default'], ['Output-token reservation', limits.default_max_tokens || 'Server default']]);
}

async function connections(p) {
  const admin = p.identity.role === 'admin';
  const person = p.identity.kind === 'person';
  p.root.append(heading(person ? 'My account' : admin ? 'Connect a client' : 'My Access', 'Connect your tools and review your access.'));
  const endpoints = connectionGuide(p);
  const identity = card(person ? 'Your account' : 'Your key', facts([['Name', p.identity.name], [person ? 'Username' : 'ID', person ? p.identity.username : p.identity.id], ['Role', p.identity.role], ['Source', person ? 'Local account' : p.identity.is_virtual ? 'Managed key' : 'Startup key'], ...(person ? [] : [['Expires', date(p.identity.expires_at)]])]));
  if (['rpm_limit', 'tpm_limit', 'max_parallel_requests', 'spend_limit', 'default_max_tokens'].some(key => p.identity.limits[key] > 0)) identity.append(el('h3', {}, 'Additional key limits'), limitFacts(p.identity.limits));
  const team = p.identity.team ? card('Your team policy', el('h3', {}, p.identity.team.name), policySummary(p.identity.team), limitFacts(p.identity.team.limits), text('Provider access and usage rules are managed by your team.', 'muted')) : card('Model access', text('This startup key has no team-specific model restrictions. Its role still determines which APIs it can use.'));
  const usage = p.identity.usage;
  const activity = card('Your recorded usage', usage ? usageDetails(usage) : text('No usage record is available for this identity.', 'muted'));
  const grants = card('Your authorizations', message('Loading your MCP grants…'));
  const policies = el('div', { class: 'cards access-overview' }, identity, team, activity);
  p.root.append(...(person ? [policies, accountSettings(p), endpoints] : admin ? [endpoints, policies] : [policies, endpoints]), grants, text('Signing out ends browser access. Revoking a client ends that MCP grant and does not sign you out of this console.', 'footer-note'));
  try {
    const entries = await api.list('/me/connections', { signal: p.scope.signal });
    if (!p.current()) return;
    grants.replaceChildren(el('h2', {}, 'Your authorizations'));
    grants.append(text('These grants allow clients to act with your access. They are not a count of connected applications.', 'muted'));
    if (!entries.length) grants.append(text('No active MCP grants for this identity.', 'empty'));
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
      const row = el('div', { class: 'row' }, el('div', {}, el('h3', {}, grant.client), text(grant.resource), text(`Authorized ${date(grant.created_at)} · Expires ${date(grant.expires_at)}`), text(`Grant ${grant.id}`, 'muted')), revoke);
      grants.append(row, feedback);
    }
  } catch (error) { if (p.current()) grants.replaceChildren(el('h2', {}, 'Your authorizations'), problem(error), leaveButton('Reload grants', () => go('connections'), api)); }
}

function readRoute() {
  const [name, query] = (location.hash || '#overview').slice(1).split('?');
  const values = new URLSearchParams(query);
  return { name: name || 'overview', item: values.get('item') || undefined, mode: values.get('mode') || undefined, node: values.get('node') || undefined, provider: values.get('provider') || undefined };
}

function writeRoute(replace = false) {
  const query = new URLSearchParams({ ...(section ? { item: section } : {}), ...(mode ? { mode } : {}), ...(computeNode ? { node: computeNode } : {}), ...(computeProvider ? { provider: computeProvider } : {}) });
  const hash = '#' + route + (query.size ? '?' + query : '');
  if (!window.history) return;
  if (!historyReady) {
    window.history.replaceState({ zzrouterConsoleIndex: historyIndex }, '', hash);
    historyReady = true;
  } else if (location.hash !== hash) {
    if (!replace) historyIndex++;
    window.history[replace ? 'replaceState' : 'pushState']({ zzrouterConsoleIndex: historyIndex }, '', hash);
  }
}

function followHistory() {
  if (!api.state?.identity) return;
  const destination = window.history?.state?.zzrouterConsoleIndex;
  if (restoringHistory) {
    if (destination === historyIndex) restoringHistory = false;
    return;
  }
  const next = readRoute();
  if (next.name === route && next.item === section && next.mode === mode && next.node === computeNode && next.provider === computeProvider && destination === historyIndex) return;
  if (!canLeave()) {
    if (Number.isInteger(destination) && destination !== historyIndex) {
      restoringHistory = true;
      window.history.go(historyIndex - destination);
    } else writeRoute(true);
    return;
  }
  historyIndex = Number.isInteger(destination) ? destination : historyIndex + 1;
  window.history?.replaceState({ zzrouterConsoleIndex: historyIndex }, '', location.hash);
  if (next.name !== route || next.item !== section || next.mode !== mode || next.node !== computeNode || next.provider !== computeProvider) void go(next.name, next.item, next.mode, { checked: true, node: next.node, provider: next.provider });
}
document.querySelector('a.skip')?.addEventListener('click', event => {
  event.preventDefault();
  const target = document.querySelector('#content');
  target?.setAttribute('tabindex', '-1');
  target?.focus();
});
window.addEventListener('popstate', followHistory);
window.addEventListener('hashchange', followHistory);

channel?.addEventListener('message', event => { if (event.data === 'auth-state-changed') void refreshSession('Browser access changed in another tab.'); });
window.addEventListener('focus', event => { if (event.target === window) void probeSession(); });
window.addEventListener('pageshow', event => { if (event.persisted) void (api.state || disclosure ? probeSession() : refreshSession()); });
window.addEventListener('beforeunload', event => { if (!api.canLeave() || leaveGuard?.dirty()) { event.preventDefault(); event.returnValue = ''; } });
window.addEventListener('pagehide', () => { authorizationRequest = null; api.clear('Session paused.'); });
void refreshSession();
