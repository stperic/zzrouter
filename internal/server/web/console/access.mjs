import { el, text, button, leaveButton, badge, actions, message, card, heading, input, facts, date, problem, selectableRow, markSelection } from './dom.mjs';
import { Lifetime } from './api.mjs';
import { details } from './details.mjs';
import { accessForm, limitSummary } from './access-form.mjs';
import { accessLabel, policySummary } from './access-policy.mjs';
import { budgetLabel, budgetUsage } from './access-budget.mjs';
export { editableValues } from './access-form.mjs';

export async function access(p, section = 'keys', mode) {
  const [requestedType, ...requestedID] = section.split('/');
  const type = requestedType === 'teams' ? 'teams' : 'keys';
  p.root.append(heading('Keys & teams', 'Named keys for your clients. Shared access and budgets for your teams.'), message('Loading access settings…'));
  let [keys, teams] = await Promise.all(['/keys', '/teams'].map(path => p.api.list(path, { signal: p.scope.signal })));
  if (!p.current()) return;
  // Optional catalogs enrich choices. Failure never substitutes an empty policy.
  const readChoices = () => Promise.allSettled(['/providers/catalog?kind=all', '/models', '/model-groups'].map(path => p.api.list(path, { signal: p.scope.signal })));
  let catalogReads = readChoices();
  let records = type === 'keys' ? keys : teams;
  let selected = requestedID.join('/');
  const workspace = el('div', { class: 'entity-workspace' });
  const listing = el('div', { class: 'entity-list access-list' });
  workspace.append(listing);
  let drawer;
  const openRecord = (kind, id) => p.go('access', `${kind}/${id}`);
  const choose = async (next = type, nextMode) => {
    selected = next.split('/').slice(1).join('/'); mode = nextMode;
    drawer?.dispose(!selected && mode !== 'create'); drawer = undefined;
    markSelection(listing, selected);
    if (!selected && mode !== 'create') return;
    const label = type === 'keys' ? 'key' : 'team';
    const owner = details({ ...p, root: workspace }, mode === 'create' ? `Create ${label}` : `${type === 'keys' ? 'Key' : 'Team'} details`, () => p.go('access', type));
    drawer = owner;
    owner.root.append(message('Loading access details…'));
    const loadingFocus = document.activeElement;
    const reload = async (id = selected, deleted = false, notice) => {
      const revision = owner.panel.revision;
      const current = () => owner.panel.current() && owner.panel.revision === revision && p.api.canLeave();
      const fresh = await Promise.all(['/keys', '/teams'].map(path => p.api.list(path, { signal: owner.panel.signal })));
      if (!current()) return;
      [keys, teams] = fresh; records = type === 'keys' ? keys : teams; render();
      if (deleted || !id) { p.selectSection?.(type); await choose(type); listing.prepend(message(deleted ? type === 'keys' ? 'Key revoked.' : 'Team deleted.' : 'Current access settings loaded.')); return; }
      if (!await load(id, notice)) return;
      if (owner.panel.current()) { selected = id; mode = undefined; p.selectSection?.(`${type}/${id}`); markSelection(listing, id); }
    };
    const load = async (id, notice) => {
      const revision = owner.panel.revision;
      const [record, choices] = await Promise.all([id ? p.api.data(`/${type}/${encodeURIComponent(id)}`, { signal: owner.panel.signal }) : null, catalogReads]);
      if (!owner.panel.current() || owner.panel.revision !== revision || !p.api.canLeave()) return false;
      const [catalog, models, groups] = choices.map(result => result.status === 'fulfilled' ? result.value : []);
      const members = keys.filter(key => key.team_id === id);
      const options = { teams, catalog, models, groups, catalogUnavailable: choices[0].status === 'rejected', modelsUnavailable: choices.slice(1).some(result => result.status === 'rejected'), memberCount: members.length };
      const show = () => {
        const reader = owner.panel.view(type === 'keys' ? 'Key details' : 'Team details');
        const team = type === 'keys' ? teams.find(team => team.id === record.team_id) : record;
        reader.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, record.name), record.kind !== 'personal' ? leaveButton('Edit', edit, p.api, 'primary') : null),
          notice ? message(notice) : '', badge(record.suspended ? 'Suspended' : record.is_expired ? 'Expired' : 'Active', record.suspended || record.is_expired ? 'warn' : 'good'),
          type === 'keys' ? facts([['Key ID', record.id], ['Role', record.role], ['Expires', date(record.expires_at)], ['Team', team?.name || 'Unavailable'], ['Team role', record.team_role || 'Member']]) : facts([['Team ID', record.id], ['Type', record.kind === 'personal' ? 'Personal' : 'Shared']]),
          record.description ? text(record.description, 'muted') : '');
        if (team) reader.root.append(el('section', { class: 'access-section' }, el('h3', {}, type === 'keys' ? 'Inherited team access' : 'Access policy'), policySummary(team, catalog),
          budgetUsage(reader, 'teams', team), el('details', {}, el('summary', {}, 'Usage limits'), limitSummary(team)),
          type === 'keys' ? leaveButton('View team', () => openRecord('teams', team.id), p.api, 'link') : null));
        if (record.kind === 'personal') reader.root.append(message('This personal team belongs to an existing key. Create a shared team and replacement key to manage its policy.'));
        if (type === 'keys' && (record.spend_limit > 0 || ['rpm_limit', 'tpm_limit', 'max_parallel_requests', 'default_max_tokens'].some(key => record[key] > 0))) reader.root.append(el('section', { class: 'access-section' }, el('h3', {}, 'Additional key limits'), budgetUsage(reader, 'keys', record), limitSummary(record)));
        if (type === 'teams') reader.root.append(el('section', { class: 'access-section' }, el('h3', {}, `Member keys (${members.length})`), members.length ? el('ul', { class: 'member-keys' }, members.map(key => el('li', {}, leaveButton(key.name || key.id, () => openRecord('keys', key.id), p.api, 'link'), badge(key.suspended ? 'Suspended' : key.is_expired ? 'Expired' : key.team_role || 'Member')))) : text('No member keys yet. Select this team when creating a key.', 'muted')));
        p.syncActivity();
      };
      const edit = () => {
        const editorOwner = owner.panel.view(record ? `Edit ${label}` : `Create ${label}`);
        let active = 'primary';
        const cancel = () => record ? show() : p.go('access', type);
        const formOwner = { ...editorOwner, closeEditor: cancel, current: () => editorOwner.current() && active === 'primary' };
        const formOptions = { ...options, readOwner: editorOwner };
        formOptions.createTeam = () => {
          if (!p.api.canLeave()) return;
          const instance = Symbol('team'); active = instance;
          const current = () => editorOwner.current() && active === instance;
          const back = team => {
            if (!p.api.canLeave() || !current()) return;
            nested.draft?.dispose(); active = 'primary';
            if (team) { editor.selectTeam(team); editor.draft?.touch(); }
            editorOwner.root.replaceChildren(editor.root); editor.focusTeam(); p.syncActivity();
          };
          const nested = accessForm({ ...editorOwner, current, closeEditor: () => back() }, 'teams', null, { ...formOptions, created: back }, async () => {
            try {
              const fresh = await p.api.list('/teams', { signal: editorOwner.scope.signal });
              if (!current()) return;
              editor.refreshTeams(fresh); back();
              editorOwner.root.prepend(message('Teams reloaded. Check the team list before creating another team. Your key has not been created.'));
            } catch (error) { if (current()) editorOwner.root.prepend(problem(error)); }
          });
          editorOwner.root.replaceChildren(text('Create a team for this key. Your key draft is kept until this panel closes.', 'muted'), nested.root);
          nested.focus(); p.syncActivity();
        };
        const editor = accessForm(formOwner, type, record, formOptions, (id, deleted) => reload(typeof id === 'string' ? id : record?.id, deleted, 'Changes saved.'));
        editorOwner.root.append(editor.root); if (document.activeElement === loadingFocus || record) editor.focus?.(); p.syncActivity();
      };
      if (record) show(); else edit();
      return true;
    };
    try { await load(mode === 'create' ? undefined : selected); }
    catch (error) {
      if (!owner.current()) return;
      if (error.status === 404) { p.selectSection?.(type); await choose(type); listing.prepend(message(`This ${label} is no longer available.`, 'warning')); }
      else owner.root.replaceChildren(problem(error), leaveButton('Reload details', () => choose(next, nextMode), p.api));
    }
  };
  const refresh = async () => {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    const panel = drawer; const revision = panel?.panel.revision;
    const unchanged = () => p.current() && p.api.canLeave() && drawer === panel && drawer?.panel.revision === revision;
    try {
      catalogReads = readChoices();
      [keys, teams] = await Promise.all(['/keys', '/teams'].map(path => p.api.list(path, { signal: p.scope.signal })));
      if (!p.current()) return;
      records = type === 'keys' ? keys : teams; render(); if (unchanged()) await choose(selected ? `${type}/${selected}` : type, mode);
    } catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  const search = input('access_search', '', { type: 'search', placeholder: type === 'keys' ? 'Search keys or teams' : 'Search teams', 'aria-label': type === 'keys' ? 'Search keys or teams' : 'Search teams' });
  const results = el('div');
  const resultsLifetime = new Lifetime();
  p.scope.signal.addEventListener('abort', () => resultsLifetime.end(), { once: true });
  let page = 0;
  const pageSize = 20;
  const render = () => {
    const resultScope = resultsLifetime.next();
    const rowsOwner = { ...p, current: () => p.current() && resultScope.current(), scope: { signal: AbortSignal.any([p.scope.signal, resultScope.signal]) } };
    const query = search.value.trim().toLowerCase();
    const filtered = records.filter(record => `${record.name} ${record.id} ${teams.find(team => team.id === record.team_id)?.name || ''}`.toLowerCase().includes(query));
    page = Math.max(0, Math.min(page, Math.max(0, Math.ceil(filtered.length / pageSize) - 1)));
    const rows = filtered.slice(page * pageSize, (page + 1) * pageSize);
    const body = el('tbody');
    for (const record of rows) {
      const team = type === 'keys' ? teams.find(team => team.id === record.team_id) : record;
      body.append(selectableRow(record.id, selected, () => openRecord(type, record.id),
        el('td', {}, leaveButton(record.name, () => openRecord(type, record.id), p.api, 'link entity-title'), text(record.id, 'muted'), record.role === 'admin' ? badge('Administrator') : null),
        el('td', {}, type === 'keys' ? team?.name || 'Team unavailable' : `${keys.filter(key => key.team_id === record.id).length} ${keys.filter(key => key.team_id === record.id).length === 1 ? 'key' : 'keys'}`, text(type === 'teams' && record.kind === 'personal' ? 'Personal' : accessLabel(team), 'muted')),
        el('td', {}, type === 'teams' ? budgetUsage(rowsOwner, type, record, { compact: true }) : [
          text(team ? budgetLabel(team) : 'Team budget unavailable'),
          record.spend_limit > 0 ? text(`Additional key budget: ${budgetLabel(record)}`, 'muted') : null,
          ['rpm_limit', 'tpm_limit', 'max_parallel_requests', 'default_max_tokens'].some(key => record[key] > 0) ? text('Additional key limits', 'muted') : null,
        ]),
        el('td', {}, badge(record.suspended ? 'Suspended' : record.is_expired ? 'Expired' : 'Active', record.suspended || record.is_expired ? 'warn' : 'good'))));
    }
    const pagination = actions(text(`${filtered.length} ${type}${filtered.length ? ` · ${page * pageSize + 1}–${page * pageSize + rows.length}` : ''}`, 'muted'));
    if (filtered.length > pageSize) {
      const previous = button('Previous', () => { if (p.api.canLeave() && page > 0) { page--; render(); } });
      const next = button('Next', () => { if (p.api.canLeave() && (page + 1) * pageSize < filtered.length) { page++; render(); } });
      previous.disabled = page === 0; next.disabled = (page + 1) * pageSize >= filtered.length;
      pagination.append(previous, next);
    }
    results.replaceChildren(rows.length ? el('div', { class: 'table-wrap' }, el('table', {}, el('thead', {}, el('tr', {}, ['Name', type === 'keys' ? 'Team & access' : 'Members & access', type === 'keys' ? 'Team budget' : 'Budget', 'Status'].map(label => el('th', { scope: 'col' }, label)))), body)) : text(query ? 'No matching results.' : type === 'keys' ? 'No managed keys yet. Create one for each person or client.' : 'No teams yet. Create a shared team to control access and usage.', 'empty'), pagination);
  };
  let searchTimer;
  p.scope.signal.addEventListener('abort', () => clearTimeout(searchTimer), { once: true });
  search.addEventListener('input', () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { if (p.current()) { page = 0; render(); } }, 150); });
  const tabs = actions(leaveButton('Keys', () => p.go('access', 'keys'), p.api), leaveButton('Teams', () => p.go('access', 'teams'), p.api));
  tabs.classList.add('tabs'); tabs.children[type === 'keys' ? 0 : 1].setAttribute('aria-current', 'true');
  listing.append(card(type === 'keys' ? 'Client keys' : 'Access teams', search, results));
  p.root.replaceChildren(heading('Keys & teams', 'Named keys for your clients. Shared access and budgets for your teams.', leaveButton('Refresh', refresh, p.api), leaveButton(type === 'keys' ? '+ Create key' : '+ Create team', () => p.go('access', type, 'create'), p.api, 'primary')), tabs, workspace,
    text('Teams define provider access, budgets and usage limits. Keys inherit their team’s policy. Startup keys are managed in installation configuration.', 'footer-note'));
  p.handleSelection?.(choose, next => (next || 'keys').split('/')[0] === type);
  render(); await choose(section, mode);
}
