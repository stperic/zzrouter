import { Lifetime } from './api.mjs';
import { el, text, button, leaveButton, actions, message, card, heading, field, input, select, facts, problem, date, selectableRow, markSelection, compactTable } from './dom.mjs';
import { details } from './details.mjs';
import { routeForm } from './route-form.mjs';
import { accessForm } from './access-form.mjs';
import { providerCategory } from './inventory.mjs';
import { providerName } from './provider-icons.mjs';
import { routeReason } from './diagnostics.mjs';

const endpoint = name => `/model-groups/${encodeURIComponent(name)}`;
const assignedTeams = (teams, name) => teams.filter(team => team.route_access?.mode === 'selected' && team.route_access.routes?.includes(name));
const description = 'Create named routes and manage team access.';

export function routeStatus(state) {
  const replicas = state?.replicas;
  if (!replicas?.length) return ['Not observed', 'unknown'];
  const healthy = replicas.filter(replica => replica.health_state === 'healthy' && !replica.cooldown_remaining_seconds).length;
  const down = replicas.filter(replica => replica.health_state === 'unhealthy' || replica.cooldown_remaining_seconds > 0).length;
  return healthy === replicas.length ? [`${healthy}/${replicas.length} healthy`, 'healthy']
    : down === replicas.length ? ['Unavailable', 'down']
    : healthy || down ? [`${healthy}/${replicas.length} healthy`, 'degraded'] : ['Health unknown', 'unknown'];
}

function healthLabel(label, tone) {
  return el('span', { class: 'route-health' }, el('span', { class: `route-dot ${tone}`, 'aria-hidden': true }), label);
}

export async function routes(p, selected, mode) {
  p.root.append(heading('Routes', description), message('Loading routes…'));
  let [records, teams] = await Promise.all(['/model-groups', '/teams'].map(path => p.api.list(path, { signal: p.scope.signal })));
  const readChoices = () => Promise.allSettled(['/providers/catalog?kind=all', '/models', '/nodes'].map(path => p.api.list(path, { signal: p.scope.signal })));
  let choices = await readChoices();
  if (!p.current()) return;
  let [catalog, models, nodes] = choices.map(result => result.status === 'fulfilled' ? result.value : []);
  const place = replica => replica.node || (catalog.some(item => item.key === replica.provider) ? providerCategory(catalog.find(item => item.key === replica.provider)) === 'cloud' ? 'Cloud' : 'Coordinator' : 'Location unclassified');
  const destinations = route => el('div', { class: 'route-targets' }, route.replicas.map(replica => el('div', {}, providerName(replica.provider, catalog.find(item => item.key === replica.provider)?.name || replica.provider), text(`${place(replica)} · ${replica.model}`, 'muted'))));
  const listing = el('div', { class: 'entity-list access-list' });
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  let drawer;
  const selectRoute = async (name, nextMode) => {
    drawer?.dispose(!name && nextMode !== 'create'); drawer = undefined;
    selected = name; mode = nextMode;
    markSelection(listing, name);
    if (!name && mode !== 'create') return;
    const owner = details({ ...p, root: workspace }, mode === 'create' ? 'Create route' : 'Route details', () => p.go('routes'));
    drawer = owner;
    owner.root.append(message('Loading route details…'));
    const reload = async (id = name, deleted = false, notice) => {
      const revision = owner.panel.revision;
      const current = () => owner.panel.current() && owner.panel.revision === revision && p.api.canLeave();
      const fresh = await Promise.all(['/model-groups', '/teams'].map(path => p.api.list(path, { signal: owner.panel.signal })));
      if (!current()) return;
      records = fresh[0]; teams = fresh[1]; render();
      if (deleted) { p.selectSection?.(undefined); await selectRoute(); listing.prepend(message('Route deleted.')); return; }
      if (!await load(id, notice)) return;
      if (owner.panel.current()) { selected = id; mode = undefined; p.selectSection?.(id); markSelection(listing, id); }
    };
    const load = async (id, notice) => {
      const revision = owner.panel.revision;
      let etag = '';
      const record = id ? await p.api.data(endpoint(id), { signal: owner.panel.signal, onVersion: value => { etag = value; } }) : null;
      if (!owner.panel.current() || owner.panel.revision !== revision || !p.api.canLeave()) return false;
      const assigned = assignedTeams(teams, id);
      const edit = () => {
        const editor = owner.panel.view(record ? 'Edit route' : 'Create route');
        const cancel = () => record ? show() : p.go('routes');
        editor.root.append(el('h2', {}, record?.name || 'Create a route'), routeForm(editor, record,
          { catalog, models, nodes, etag, teams: assigned, incomplete: choices.some(result => result.status === 'rejected'), cancel, saved: (name, deleted) => reload(name, deleted, deleted ? undefined : 'Route saved.') }, () => reload(id)));
        editor.root.querySelector?.('input')?.focus({ preventScroll: true });
        p.syncActivity();
      };
      const editTeam = target => {
        const editor = owner.panel.view(target ? 'Edit team route access' : 'Create team for route');
        const saved = target ? 'Team route access saved.' : 'Team created.';
        const assignment = accessForm({ ...editor, closeEditor: () => show(true) }, 'teams', target,
          { catalog, models, groups: records, route: record.name, catalogUnavailable: choices[0].status === 'rejected', modelsUnavailable: choices[1].status === 'rejected' },
          teamID => reload(id, false, teamID ? saved : undefined));
        editor.root.append(assignment.root);
        assignment.focus();
        p.syncActivity();
      };
      const chooseTeam = () => {
        const picker = owner.panel.view('Assign route to team');
        const shared = teams.filter(team => team.kind === 'shared');
        const cancel = leaveButton('Cancel', () => show(true), p.api);
        picker.root.append(el('h2', {}, record.name));
        if (!shared.length) {
          const create = leaveButton('Create team', () => editTeam(null), p.api, 'primary');
          picker.root.append(message('Create a shared team to assign this route.'), actions(create, cancel));
          create.focus();
          return;
        }
        const team = select('assignment_team', [['', 'Choose a team…'], ...shared.map(team => [
          team.id, `${team.name}${assigned.includes(team) ? ' (assigned)' : ''}${team.suspended ? ' (suspended)' : ''}`,
        ])], '');
        team.required = true;
        const form = el('form', { class: 'entity-editor' }, field('Shared team', team),
          actions(el('button', { type: 'submit', class: 'primary', 'data-leave-view': true }, 'Continue'), cancel));
        form.addEventListener('submit', event => {
          event.preventDefault();
          if (!picker.current() || !p.api.canLeave() || !form.reportValidity()) return;
          const target = shared.find(item => item.id === team.value);
          if (target) editTeam(target);
        });
        picker.root.append(form);
        team.focus();
      };
      const show = (focusAssignment = false) => {
        const reader = owner.panel.view('Route details');
        const assign = leaveButton(assigned.length ? 'Manage team assignments' : 'Assign to team', chooseTeam, p.api);
        reader.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, record.name), actions(leaveButton('Try in Chat', () => p.go('chat', record.name), p.api), leaveButton('Edit', edit, p.api, 'primary'))),
          notice ? message(notice) : '', text(record.description || 'No description', 'muted'),
          facts([['Selection', { priority: 'Priority order', 'least-load': 'Least loaded', fastest: 'Fastest observed' }[record.strategy] || record.strategy], ['Management', record.auto_managed ? 'Automatic' : record.auto_owner ? 'Claimed automatic route' : 'Manual'], ['Expires', date(record.expires_at)]]),
          el('section', { class: 'access-section' }, el('h3', {}, `Assigned teams (${assigned.length})`),
            assigned.length ? actions(assigned.map(team => leaveButton(team.name, () => p.go('access', `teams/${team.id}`), p.api, 'link'))) : text('Teams with direct model and route access may already use this route.', 'muted'),
            assign),
          el('section', { class: 'access-section' }, el('h3', {}, 'Destinations'), destinations(record)),
          routePreview(reader, record, teams, assigned, etag).root);
        if (focusAssignment) assign.focus();
      };
      if (record) show(); else edit();
      return true;
    };
    try { await load(mode === 'create' ? undefined : name); }
    catch (error) {
      if (!owner.current()) return;
      if (error.status === 404) { p.selectSection?.(undefined); await selectRoute(); listing.prepend(message('This route is no longer available.', 'warning')); }
      else owner.root.replaceChildren(problem(error), leaveButton('Reload details', () => selectRoute(name, mode), p.api));
    }
  };
  const refresh = async () => {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    const panel = drawer; const revision = panel?.panel.revision;
    const unchanged = () => p.current() && p.api.canLeave() && drawer === panel && drawer?.panel.revision === revision;
    try {
      const [fresh, inventory] = await Promise.all([Promise.all(['/model-groups', '/teams'].map(path => p.api.list(path, { signal: p.scope.signal }))), readChoices()]);
      [records, teams] = fresh; choices = inventory;
      [catalog, models, nodes] = choices.map(result => result.status === 'fulfilled' ? result.value : []);
      if (!p.current()) return;
      render(); if (unchanged()) await selectRoute(selected, mode);
    } catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  const search = input('route_search', '', { type: 'search', placeholder: 'Search routes, providers or teams', 'aria-label': 'Search routes' });
  const results = el('div');
  const reads = new Lifetime();
  p.scope.signal.addEventListener('abort', () => reads.end(), { once: true });
  let page = 0;
  const render = () => {
    const scope = reads.next();
    const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    const query = search.value.trim().toLowerCase();
    const filtered = records.filter(route => `${route.name} ${route.description || ''} ${route.replicas.map(replica => `${replica.provider} ${replica.model} ${replica.node}`).join(' ')} ${assignedTeams(teams, route.name).map(team => team.name).join(' ')}`.toLowerCase().includes(query));
    page = Math.max(0, Math.min(page, Math.ceil(filtered.length / 20) - 1));
    const body = el('tbody');
    for (const route of filtered.slice(page * 20, page * 20 + 20)) {
      const status = el('span', {}, healthLabel('Reading status…', 'unknown'));
      const assigned = assignedTeams(teams, route.name);
      const open = () => p.go('routes', route.name);
      body.append(selectableRow(route.name, selected, open,
        el('td', {}, leaveButton(route.name, open, p.api, 'link entity-title'), text(route.description || 'No description', 'muted')),
        el('td', {}, destinations(route)), el('td', {}, status),
        el('td', {}, assigned.length ? assigned.map(team => text(team.name)) : text('No assignments', 'muted'))));
      void p.api.data(`${endpoint(route.name)}/state`, { signal }).then(state => {
        if (p.current() && scope.current()) status.replaceChildren(healthLabel(...routeStatus(state)));
      }).catch(() => { if (p.current() && scope.current()) status.replaceChildren(healthLabel('Status unavailable', 'unknown')); });
    }
    const previous = button('Previous', () => { page--; render(); }); previous.disabled = page === 0;
    const next = button('Next', () => { page++; render(); }); next.disabled = (page + 1) * 20 >= filtered.length;
    results.replaceChildren(filtered.length ? compactTable(['Route', 'Destinations', 'Status', 'Assigned teams'], body) :
      message(query ? 'No matching routes.' : 'Create a route, then assign it to a team. Clients can use its name as their model.'),
    actions(text(`${filtered.length} ${filtered.length === 1 ? 'route' : 'routes'}`, 'muted'), filtered.length > 20 ? [previous, next] : null));
  };
  let timer;
  search.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(() => { if (p.current()) { page = 0; render(); } }, 150); });
  p.scope.signal.addEventListener('abort', () => clearTimeout(timer), { once: true });
  listing.append(card('Your routes', search, results));
  p.root.replaceChildren(heading('Routes', description,
    leaveButton('Refresh', refresh, p.api), leaveButton('+ Create route', () => p.go('routes', undefined, 'create'), p.api, 'primary')), workspace);
  p.handleSelection?.(selectRoute);
  render(); await selectRoute(selected, mode);
}

function routePreview(p, route, teams, assigned, etag) {
  let invalidated = false;
  const scope = new AbortController();
  const form = el('form', { class: 'access-section route-preview' });
  const controls = el('fieldset');
  const team = select('preview_team', [['', 'Choose a team…'], ...teams.map(item => [item.id, item.name])], assigned[0]?.id || '');
  team.required = true;
  const inputTokens = input('preview_input_tokens', 1000, { type: 'number', min: 0, step: 1, required: true });
  const outputTokens = input('preview_output_tokens', 500, { type: 'number', min: 0, step: 1, required: true });
  const result = el('div', { 'aria-live': 'polite' });
  const run = el('button', { type: 'submit', class: 'secondary' }, 'Preview route');
  controls.append(field('Team', team), el('details', {}, el('summary', {}, 'Cost projection'),
    el('div', { class: 'form-grid' }, field('Input tokens', inputTokens), field('Output tokens', outputTokens))), run,
    text('Uses the saved route. Checks team permissions and budget without sending a request or starting a model.', 'muted'));
  for (const control of [team, inputTokens, outputTokens]) control.addEventListener('change', () => result.replaceChildren());
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!form.reportValidity() || !team.value || controls.disabled || !p.api.canChange()) return;
    controls.disabled = true;
    result.replaceChildren(message('Computing preview…'));
    try {
      const preview = await p.api.data(`${endpoint(route.name)}/preview`, { method: 'POST', effectful: false,
        body: { team_id: team.value, input_tokens: Number(inputTokens.value), output_tokens: Number(outputTokens.value) }, signal: AbortSignal.any([p.scope.signal, scope.signal]) });
      if (!p.current() || invalidated) return;
      const assignment = preview.assignments?.[0];
      if (!assignment) { result.replaceChildren(message('The route is no longer available.', 'warning')); return; }
      if (assignment.route_version !== etag) { invalidate(); return; }
      const label = name => { const target = route.replicas.find(replica => replica.name === name); return target ? `${target.provider} · ${target.model}${target.node ? ` on ${target.node}` : ''}` : name; };
      result.replaceChildren(text(`Preview for ${teams.find(value => value.id === team.value)?.name || team.value}`, 'muted'), assignment.replica ? message(`Preferred destination: ${label(assignment.replica)}`) : message(assignment.denied_reason ? `Blocked: ${routeReason(assignment.denied_reason)} (${assignment.denied_reason})` : 'No eligible destination.', 'warning'));
      if (assignment.eligible?.length > 1) result.append(text(`Next choices: ${assignment.eligible.slice(1).map(label).join('; ')}`, 'muted'));
      if (assignment.replica) result.append(text(assignment.projected_cost_source ? `Projected cost: $${(assignment.projected_cost_micro / 1e6).toFixed(6)}` : 'Projected cost unavailable.', 'muted'));
      if (assignment.skipped?.length) result.append(el('ul', {}, assignment.skipped.map(skip => el('li', {}, `${label(skip.replica)}: ${routeReason(skip.reason)} (${skip.reason})`))));
      result.append(text(`Not checked: ${(preview.unchecked || []).map(value => value.replaceAll('_', ' ')).join(', ')}. Live requests recheck current conditions.`, 'muted'));
    } catch (error) { if (p.current() && !invalidated) result.replaceChildren(problem(error)); }
    finally { if (p.current()) controls.disabled = invalidated; }
  });
  const invalidate = () => {
    invalidated = true; scope.abort(); controls.disabled = true;
    result.replaceChildren(message('Reload the route to preview its current destinations.', 'warning'));
  };
  form.append(controls, result);
  return { root: el('details', { class: 'route-preview-section' }, el('summary', {}, 'Preview as team'), form), invalidate };
}
