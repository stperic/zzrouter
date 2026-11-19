import { el, text, button, leaveButton, field, input, facts, message, problem, clearSecrets } from './dom.mjs';
import { formSubmission } from './details.mjs';

// Consume an invitation before bootstrap or navigation can retain its secret.
export function takeAccessLink(location, history) {
  const match = /^#(invite|recover)\?(.*)$/.exec(location.hash || '');
  if (!match) return null;
  const token = new URLSearchParams(match[2]).get('token');
  history.replaceState(history.state, '', location.pathname + location.search);
  return token && token.length <= 512 ? { token, purpose: match[1] === 'invite' ? 'invitation' : 'recovery' } : null;
}

// This is a continuation to the router's consent page, never a general return URL.
// Keep the request only in memory, before bootstrap can observe or retain it.
export function takeAuthorizationLink(location, history) {
  const match = /^#authorize\?(.*)$/.exec(location.hash || '');
  if (!match) return null;
  history.replaceState(history.state, '', location.pathname + location.search);
  const values = new URLSearchParams(match[1]).getAll('request');
  const request = values.length === 1 ? values[0] : '';
  if (!request.startsWith('/oauth/authorize?') || request.length > 16384 || /[#\\\u0000-\u0020\u007f]/.test(request)) return null;
  try {
    const url = new URL(request, location.origin);
    if (url.origin !== location.origin || url.pathname !== '/oauth/authorize' || !url.search || url.hash) return null;
    return url.pathname + url.search;
  } catch { return null; }
}

export function passwordControl(name, autocomplete = 'new-password') {
  const control = input(name, '', { type: 'password', required: true, autocomplete, 'data-sensitive': true });
  const reveal = button('Show', () => {
    const shown = control.type === 'password';
    control.type = shown ? 'text' : 'password';
    reveal.textContent = shown ? 'Hide' : 'Show';
    reveal.setAttribute('aria-pressed', String(shown));
    reveal.setAttribute('aria-label', shown ? 'Hide password' : 'Show password');
  }, 'password-reveal');
  reveal.setAttribute('aria-label', 'Show password');
  reveal.setAttribute('aria-pressed', 'false');
  return { control, field: label => el('div', { class: 'password-field' }, field(label, control), reveal) };
}

export function validNewPassword(value) { return Array.from(value).length >= 12 && Array.from(value).length <= 128; }

export async function signIn(p, incoming) {
  const { api, state } = p;
  const body = el('div', { class: 'sign-in-body' });
  p.root.append(body);
  let grant;
  p.scope.signal.addEventListener('abort', () => { grant = undefined; incoming = undefined; clearSecrets(body); }, { once: true });
  const reset = () => { grant = undefined; clearSecrets(body); return p.refresh(); };
  const form = (title, description, fields, submitLabel, request, { success = p.signedIn, enrollment = false } = {}) => {
    clearSecrets(body); body.replaceChildren(el('h1', {}, title), text(description, 'muted'));
    const feedback = el('div', { 'aria-live': 'polite' });
    const controls = el('fieldset', { 'data-protected-actions': true }, fields);
    const submit = el('button', { type: 'submit', class: 'primary' }, submitLabel);
    const node = el('form', {}, controls, submit, feedback);
    const submission = formSubmission(p, controls, submit);
    node.addEventListener('submit', async event => {
      event.preventDefault();
      if (!node.reportValidity()) return;
      await submission.run(request, success, (error, blocked) => {
        feedback.replaceChildren(problem(error));
        if (blocked && enrollment) feedback.append(message('Your account may already be saved. Try signing in with the username and password you chose. This invitation or setup code will not be submitted again.', 'warning'));
        if (blocked || error.status === 403) feedback.append(leaveButton('Return to sign in', reset, api));
      });
    });
    body.append(node);
    p.syncActivity?.();
    return { node, feedback };
  };
  const post = (path, data) => api.data(path, { method: 'POST', body: data, signal: p.scope.signal, notifyAuth: false, effectful: path !== '/access/invitations/inspect' });
  const enterAccount = () => {
    const recovering = grant.purpose === 'recovery';
    const owner = grant.purpose === 'setup';
    const name = input('name', '', { required: !recovering, autocomplete: 'name', maxlength: '100' });
    const username = input('username', '', { required: !recovering, autocomplete: 'username', autocapitalize: 'none', spellcheck: 'false', maxlength: '64' });
    const password = passwordControl('password');
    const fields = [recovering ? null : field('Name', name), recovering ? null : field('Username', username), password.field(recovering ? 'New password' : 'Password'),
      text('Use a passphrase of 12 to 128 characters. This account is stored on your router.', 'field-hint')];
    const path = recovering ? '/access/recovery' : owner ? '/access/setup' : '/access/invitations/accept';
    const title = recovering ? 'Choose a new password' : owner ? 'Create your owner account' : 'Create your account';
    const panel = form(title, recovering ? 'Your other sessions will stop working when the password changes.' : owner ? 'Manage your router with a local account. No email or cloud account is required.' : 'Your team controls which providers and routes you can use.', fields,
      recovering ? 'Save password and sign in' : 'Create account', async () => {
        if (!validNewPassword(password.control.value)) throw new Error('Use a password with 12 to 128 characters.');
        const data = { token: grant.token, password: password.control.value, ...(!recovering ? { username: username.value, name: name.value } : {}) };
        password.control.value = '';
        return post(path, data);
      }, { enrollment: true });
    panel.node.append(leaveButton('Return to sign in', reset, api, 'link'));
  };
  const exchange = async token => {
    const result = await post('/access/console-grants/exchange', { token });
    if (!p.current()) return;
    if (result.session) return p.signedIn();
    if (!result.token || !['setup', 'invitation', 'recovery'].includes(result.purpose)) throw new Error('The router returned an invalid setup response. Return to sign in.');
    grant = result;
    enterAccount();
  };
  const temporaryCode = (purpose = 'entry') => {
    const token = input('setup_code', '', { type: 'password', required: true, autocomplete: 'off', spellcheck: 'false', maxlength: '512', 'data-sensitive': true });
    const title = { setup: 'Set up your router', recovery: 'Recover your account', entry: 'Use a temporary code' }[purpose];
    const command = purpose === 'recovery' ? 'zzrouter access recover-owner' : 'zzrouter console';
    const panel = form(title, `Run ${command} on the installed computer, then enter the temporary code it shows.`,
      [field('Temporary code', token)], 'Continue', async () => {
        const value = token.value.trim(); token.value = '';
        await exchange(value);
      }, { success: () => {} });
    panel.node.append(text('The code is single-use and expires after 10 minutes.', 'footer-note'), leaveButton('Back to sign in', normalSignIn, api, 'link'));
  };
  const keySignIn = () => {
    const key = input('key', '', { type: 'password', required: true, autocomplete: 'off', spellcheck: 'false', maxlength: '8192', 'data-sensitive': true });
    const panel = form('Sign in with an access key', 'Use a zzRouter user or administrator key. Cloud provider keys belong in Providers after sign-in.',
      [field('Router API key', key)], 'Sign in', async () => {
        const value = key.value; key.value = '';
        return post('/session', { key: value });
      });
    panel.node.append(leaveButton('Back to account sign-in', normalSignIn, api, 'link'));
  };
  const help = () => {
    clearSecrets(body);
    body.replaceChildren(el('h1', {}, 'Get help signing in'),
      text('If you are a teammate, ask an administrator for a recovery invitation.'),
      el('h2', {}, 'Recover the owner account'), text('On the installed router computer, run:'),
      el('pre', {}, el('code', {}, 'zzrouter access recover-owner')),
      text('A system service may require administrative privileges. Use the temporary recovery code in this console. Recovery keeps your providers, models, teams and app keys.'),
      button('Enter recovery code', () => temporaryCode('recovery'), 'primary'), leaveButton('Back to sign in', normalSignIn, api, 'link'));
  };
  const normalSignIn = () => {
    grant = undefined;
    const username = input('username', '', { required: true, autocomplete: 'username', autocapitalize: 'none', spellcheck: 'false', maxlength: '64' });
    const password = passwordControl('password', 'current-password');
    const panel = form('Sign in to your router', state.node_name || 'Your private AI gateway', [field('Username', username), password.field('Password')], 'Sign in', async () => {
      const data = { username: username.value, password: password.control.value }; password.control.value = '';
      return post('/session/password', data);
    });
    panel.node.append(leaveButton('Get help signing in', help, api, 'link'));
    body.append(el('details', { class: 'advanced-access' }, el('summary', {}, 'Advanced access'),
      leaveButton('Use an access key', keySignIn, api), leaveButton('Use a temporary code', () => temporaryCode(), api)),
      text('New here? Ask your router administrator for an invitation.', 'footer-note'));
  };

  if (state.mismatch) {
    body.append(el('h1', {}, 'Open your router’s address'), message('This address does not match the configured browser origin. Open the exact public URL before entering credentials.', 'warning'), el('a', { href: state.issuer + '/console/' }, 'Continue at ' + state.issuer));
    return;
  }
  if (incoming) {
    body.append(el('h1', {}, 'Review your invitation'), message('Checking the invitation…'));
    try {
      const info = await post('/access/invitations/inspect', { token: incoming.token });
      if (!p.current()) return;
      if (info.purpose !== incoming.purpose) throw new Error('This link does not match the invitation purpose. Ask for a new link.');
      const secret = incoming.token; incoming = undefined;
      body.replaceChildren(el('h1', {}, info.purpose === 'recovery' ? 'Reset your password' : 'You’re invited'),
        text(`Continue on ${state.node_name || 'this router'}.`), facts([['Team', info.team_id || 'Owner workspace'], ['Role', info.role || 'Existing account'], ['Expires', new Date(info.expires_at).toLocaleString()]]),
        ...(state.identity ? [message('Continuing will replace the account signed in to this browser.', 'warning')] : []),
        text('This link grants access on this router. It does not verify an email address.', 'muted'));
      const feedback = el('div');
      const proceed = button('Continue', async () => {
        if (proceed.disabled || !api.canChange()) return;
        proceed.disabled = true;
        try { await exchange(secret); }
        catch (error) { if (p.current()) feedback.replaceChildren(problem(error), leaveButton('Return to sign in', reset, api)); }
      }, 'primary');
      proceed.setAttribute('data-protected-actions', '');
      body.append(proceed, feedback, leaveButton('Return to sign in', reset, api, 'link'));
    } catch (error) {
      if (p.current()) body.replaceChildren(el('h1', {}, 'Invitation unavailable'), problem(error), leaveButton('Return to sign in', reset, api));
    }
    return;
  }
  body.append(message('Checking your router…'));
  try {
    const setup = await api.data('/access/setup', { signal: p.scope.signal, notifyAuth: false });
    if (!p.current()) return;
    if (setup.initialized === false) {
      temporaryCode('setup');
      body.append(el('details', { class: 'advanced-access' }, el('summary', {}, 'Existing installation'), leaveButton('Use an access key', keySignIn, api)));
    } else normalSignIn();
  } catch (error) {
    if (p.current()) body.replaceChildren(el('h1', {}, 'Sign-in needs attention'), problem(error), leaveButton('Retry', reset, api));
  }
}
