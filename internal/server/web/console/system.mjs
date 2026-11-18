import { sharingReadiness } from './sharing.mjs';
import { el, text, leaveButton, card, heading, facts, copyField, actions, date, message, problem } from './dom.mjs';
import { pricing } from './pricing.mjs';
import { updates } from './updates.mjs';
import { auditHistory } from './audit.mjs';
import { recovery } from './recovery.mjs';

export async function system(p, selected = 'general', mode) {
  const [tab, ...rest] = selected.split('/');
  p.root.append(heading('System', 'Software, pricing and setup for your router.'),
    el('div', { class: 'tabs', 'aria-label': 'System views' }, [['general', 'General'], ['setup', 'Guided setup'], ['updates', 'Software'], ['pricing', 'Pricing'], ['audit', 'Access history'], ['recovery', 'Recovery']].map(([key, label]) => {
      const control = leaveButton(label, () => p.go('settings', key), p.api, key === tab ? 'active' : 'secondary');
      if (key === tab) control.setAttribute('aria-current', 'page'); return control;
    })));
  if (tab === 'pricing') await pricing(p, rest.join('/'), mode);
  else if (tab === 'updates') await updates(p, rest.length ? decodeURIComponent(rest.join('/')) : undefined, mode);
  else if (tab === 'setup') await setup(p);
  else if (tab === 'audit') await auditHistory(p);
  else if (tab === 'recovery') await recovery(p);
  else p.root.append(
    card('Coordinator', facts([['Name', p.state.node_name], ['Session expires', date(p.state.expires_at)]]), copyField('Console address', p.state.issuer + '/console/'), sharingReadiness(p, { settings: true })),
    card('Provider credentials', text('Manage connections and API keys in Providers.'), leaveButton('Manage providers', () => p.go('providers'), p.api),
      el('details', { class: 'access-section' }, el('summary', {}, 'Secure storage setup'), text('Protected storage is prepared automatically when the node first starts. An explicit security.credentials.key_file can use an externally managed key. Keep a separate backup of the protection key; restore it with its matching encrypted state.'))),
    card('Installation and recovery', text('Models, runtime installation and updates are available in this console. Initial node startup, listener/TLS changes and recovery require local terminal access.'),
      actions(leaveButton('Guided setup', () => p.go('settings', 'setup'), p.api), leaveButton('Software updates', () => p.go('settings', 'updates'), p.api)),
      el('a', { href: '/openapi.yaml' }, 'View the API contract')));
}

async function setup(p) {
  const root = card('Set up your router', message('Reading your current setup…')); p.root.append(root);
  const paths = ['/nodes', '/providers', '/models', '/teams'];
  const data = await Promise.allSettled(paths.map(path => p.api.list(path, { signal: p.scope.signal, timeoutMs: 45000 })));
  if (!p.current()) return;
  const summary = (index, describe) => data[index].status === 'fulfilled' ? describe(data[index].value) : 'Observation unavailable';
  const steps = [
    ['1. Review compute nodes', summary(0, rows => `${rows.length} known nodes. ${rows.filter(row => row.health_status === 'healthy').length} reported healthy.`), 'nodes', undefined, 'Inspect hardware or pair another node.'],
    ['2. Connect providers', summary(1, rows => `${rows.filter(row => row.enabled).length} enabled provider instances reported.`), 'providers', undefined, 'Install a local runtime or connect a cloud service.'],
    ['3. Choose models', summary(2, rows => `${rows.length} models in the current catalog.`), 'models', undefined, 'Discover models, review capacity and download to selected nodes.'],
    ['4. Define access', summary(3, rows => `${rows.filter(row => row.kind !== 'personal').length} shared teams reported.`), 'access', 'teams', 'Set provider and route access, budgets and limits, then invite people or issue named app keys.'],
    ['5. Try your setup', 'Run an explicit test when you are ready.', 'chat', undefined, 'Choose an authorized model or route and send a message.'],
  ];
  root.replaceChildren(el('h2', {}, 'Set up your router'), text('Each step opens the corresponding workspace. These are current observations, not proof that inference will succeed.', 'muted'),
    ...steps.map(([title, state, page, item, description]) => el('section', { class: 'setup-step' }, el('div', {}, el('h3', {}, title), text(state), text(description, 'muted')), leaveButton('Open', () => p.go(page, item), p.api))));
  data.forEach(result => { if (result.status === 'rejected') root.append(problem(result.reason)); });
}
