import { el, text, leaveButton, actions, badge, heading, facts, field, input, message, problem, date, clearSecrets, markSelection, editorActions, formFeedback, invalidField } from './dom.mjs';
import { readInventory, providerCategory } from './inventory.mjs';
import { networkNodes, nodeHealth, localProviders, localProviderKey, hardwareLabel, memoryLabel, nodeKey } from './network-data.mjs';
import { details, readAfterWrite } from './details.mjs';
import { providerName } from './provider-icons.mjs';
import { memorySummary, memoryDetails, activityText } from './observations.mjs';
import { providerDiagnostics } from './diagnostics.mjs';

export function nodeTile(p, node, destination = 'nodes', coordinator = false) {
  const item = leaveButton('', () => p.go(destination, nodeKey(node.name)), p.api, `network-item ${node.health_status === 'down' ? 'is-down' : ''}`);
  item.dataset.node = node.name;
  item.setAttribute('data-selection', nodeKey(node.name));
  if (coordinator) item.append(el('span', { class: 'network-role' }, 'Coordinator'));
  item.append(
    el('span', { class: 'network-item-title' }, el('strong', {}, node.name), badge(...nodeHealth(node))),
    el('span', { class: 'network-meta' }, [node.os || 'OS not reported', node.name === p.state.node_name && !coordinator ? 'Coordinator + Worker' : coordinator ? 'Coordination & routing' : 'Worker'].join(' · ')),
    el('span', { class: 'network-hardware' }, hardwareLabel(node)), el('span', { class: 'memory-summary' }, memorySummary(node)), activityText(p, node.activity));
  return item;
}

export async function nodes(p, selection) {
  let inventory, drawer;
  const listing = el('div', { class: 'entity-list' });
  const workspace = el('div', { class: 'entity-workspace' }, listing);
  const selectNode = next => {
    drawer?.dispose(!next); drawer = undefined; selection = next;
    markSelection(listing, next);
    if (!next) return;
    const owner = details({ ...p, root: workspace, reloadNode: async name => {
      const revision = owner.panel.revision;
      await read();
      if (owner.panel.current() && owner.panel.revision === revision && p.api.canLeave()) { p.selectSection?.(nodeKey(name)); selectNode(nodeKey(name)); }
    } }, next === 'add' ? 'Add node' : 'Node details', () => p.go('nodes'));
    drawer = owner;
    if (next === 'add') addNode(owner);
    else if (next.startsWith('node/')) showNode(owner, next.slice(5), inventory);
  };
  const read = async () => {
    const fresh = await readInventory(p);
    if (!p.current()) return;
    if (fresh[0].status === 'rejected') throw fresh[0].reason;
    inventory = fresh;
    const known = networkNodes(inventory);
    listing.replaceChildren(el('div', { class: 'node-grid' }, known.map(node => nodeTile(p, node))));
    if (!known.length) listing.append(message('No nodes reported. Add a node to begin pairing.'));
    markSelection(listing, selection);
  };
  const refresh = async () => {
    if (!(p.canLeave?.() ?? p.api.canLeave())) return;
    const panel = drawer; const revision = panel?.panel.revision;
    const unchanged = () => p.current() && p.api.canLeave() && drawer === panel && drawer?.panel.revision === revision;
    try { await read(); if (unchanged()) selectNode(selection); }
    catch (error) { if (p.current()) listing.prepend(problem(error)); }
  };
  p.root.append(heading('Nodes', 'Machines running zzRouter. The coordinator can also provide local compute.',
    leaveButton('Refresh', refresh, p.api), leaveButton('+ Add node', () => p.go('nodes', 'add'), p.api, 'primary')), workspace);
  listing.append(message('Reading node inventory…'));
  await read();
  if (p.current()) { p.handleSelection?.(selectNode); selectNode(selection); }
}

export function showNode(p, name, inventory) {
  p.editing = false;
  const node = networkNodes(inventory).find(node => node.name === name);
  if (!node) { p.root.append(message('This node is not in the current inventory.', 'warning')); return; }
  p.root.append(el('div', { class: 'detail-heading' }, el('h2', {}, node.name), leaveButton('Edit', () => editNode(p, node, inventory), p.api, 'primary')), actions(badge(node.name === p.state.node_name ? 'Coordinator + Worker' : 'Worker'), badge(...nodeHealth(node))),
    facts([['Last seen', node.last_seen_at ? date(node.last_seen_at) : 'Not reported'], ['Hardware', hardwareLabel(node)], ['Memory', memoryLabel(node)],
      ['Operating system', node.os], ['Router version', node.version], ['Node address', node.address || node.ip_address]]));
  if (node.last_error) p.root.append(message(node.last_error, 'warning'));
  else if (node.health_status !== 'healthy') p.root.append(message('The cluster connection is not currently healthy and no failure cause was reported. Check that the node is running, paired and reachable.', 'warning'));
  p.root.append(activityText(p, node.activity), memoryDetails(node), leaveButton('View workloads', () => p.go('workloads', `node/${encodeURIComponent(node.name)}`, 'runs'), p.api));
  if (node.health_status !== 'healthy') p.root.append(text('Provider and hardware details are the last reported observations.', 'muted'));
  p.root.append(el('h3', {}, 'Local providers'));
  if (inventory[1].status === 'rejected') p.root.append(problem(inventory[1].reason));
  const providers = localProviders(node, inventory);
  for (const { key, record, report } of providers) {
    const row = el('div', { class: 'node-provider-row' }, providerName(key, report?.name || key, 'strong'), actions(
      badge(record ? record.enabled ? 'Enabled' : 'Disabled' : 'Enablement unknown'),
      badge(record ? record.installed ? 'Installed' : 'Not installed' : 'Installation unknown'),
      typeof report?.running === 'boolean' ? badge(report.running ? 'Service running' : 'Service stopped', report.running ? 'good' : 'warn') : null));
    row.append(leaveButton('Provider details', () => p.go('overview', localProviderKey(node.name, key)), p.api, 'link'));
    p.root.append(row);
  }
  if (!providers.length) p.root.append(text(inventory[1].status === 'fulfilled' ? 'No local providers reported.' : 'Local provider inventory is unavailable.', 'muted'));
}

export function showLocalProvider(p, name, key, inventory) {
  p.editing = false;
  const node = networkNodes(inventory).find(node => node.name === name);
  const provider = node && localProviders(node, inventory).find(row => row.key === key);
  if (!provider) { p.root.append(message('This provider is no longer reported on this node.', 'warning')); return; }
  const { record, report } = provider;
  p.root.append(providerName(key, report?.name || key, 'h2'), text(`Local provider on ${node.name}`, 'muted'),
    facts([['Node', node.name], ['Node health', nodeHealth(node)[0]],
      ['Enablement', record ? record.enabled ? 'Enabled' : 'Disabled' : 'Unknown'],
      ['Installation', record ? record.installed ? 'Installed' : 'Not installed' : 'Unknown'],
      ['Service', typeof report?.running === 'boolean' ? report.running ? 'Service running' : 'Service stopped' : 'Not reported'],
      ['Last seen', node.last_seen_at ? date(node.last_seen_at) : 'Not reported']]));
  if (node.health_status !== 'healthy') p.root.append(message('This node is not currently healthy. Provider details are last reported observations.', 'warning'));
  if (inventory[1].status === 'rejected') p.root.append(problem(inventory[1].reason));
  p.root.append(providerDiagnostics(p, record || report, node.name), activityText(p, node.provider_activity?.[key]));
  p.root.append(actions(leaveButton('Edit node settings', () => editNode(p, node, inventory, owner => showLocalProvider(owner, name, key, inventory)), p.api, 'primary'),
    leaveButton('View node', () => p.go('overview', nodeKey(node.name)), p.api), leaveButton('View workloads', () => p.go('workloads', `node/${encodeURIComponent(node.name)}`, 'runs'), p.api)));
}

function editNode(p, node, inventory, renderDetails = owner => showNode(owner, node.name, inventory)) {
  if (p.panel) p = { ...p, ...p.panel.view('Edit node') };
  p.editing = true;
  const form = el('form', { class: 'entity-editor' });
  const draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const feedback = el('div', { 'aria-live': 'polite' });
  const entries = localProviders(node, inventory).filter(row => row.record);
  let blocked = false;
  const changes = new Map();
  const fields = new Map();
  const validation = formFeedback(form, feedback, key => fields.get(key));
  controls.append(el('h2', {}, `Settings for ${node.name}`), text('Choose which local providers this node can use. Node identity is established during pairing.', 'muted'));
  for (const { key, record, report } of entries) {
    const enabled = input(key, '', { type: 'checkbox', checked: record.enabled });
    fields.set(key, enabled);
    enabled.addEventListener('change', () => { if (enabled.checked === record.enabled) changes.delete(key); else changes.set(key, enabled.checked); save.disabled = !changes.size; });
    controls.append(field(providerName(key, report?.name || key), enabled, record.installed ? 'Installed' : 'Not installed on this node'));
  }
  const save = el('button', { type: 'submit', class: 'primary', disabled: true }, 'Save changes');
  const footer = editorActions(save, () => {
    p.root.replaceChildren(); renderDetails(p.panel ? { ...p, ...p.panel.view('Node details') } : p);
  }, p.api, draft);
  if (!entries.length) controls.append(message('Provider settings are unavailable until this node reports its providers.'));
  form.append(feedback, controls, footer);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (blocked || controls.disabled || !p.current() || !p.api.canChange() || !changes.size) return;
    controls.disabled = true;
    try {
      // Recheck only edited values. The underlying provider API has no revision CAS.
      const fresh = await p.api.list(`/providers?node=${encodeURIComponent(node.name)}`, { signal: p.scope.signal });
      if (!p.current()) return;
      for (const key of changes.keys()) {
        const before = entries.find(row => row.key === key).record;
        const observed = fresh.find(row => row.node === node.name && row.name === key && providerCategory(row) === 'local');
        if (!observed || observed.enabled !== before.enabled) throw new Error('Provider settings changed. Reload the node before editing again.');
      }
      for (const [key, enabled] of changes) {
        feedback.replaceChildren(message(`Saving ${key}…`));
        await p.api.data(`/providers/${encodeURIComponent(key)}?node=${encodeURIComponent(node.name)}`, { method: 'PATCH', body: { enabled }, signal: p.scope.signal });
        if (!p.current()) return;
      }
      blocked = true;
      draft?.clean();
      await readAfterWrite(p, () => p.reloadNode ? p.reloadNode(node.name) : p.go('nodes', nodeKey(node.name)), 'Provider settings saved.');
    } catch (error) {
      if (!p.current()) return;
      blocked = true;
      validation.error(error);
      feedback.append(message('Some changes may have completed. Reload current settings before making another change.', 'warning'), leaveButton('Reload node', () => p.reloadNode ? p.reloadNode(node.name) : p.go('nodes', nodeKey(node.name)), p.api));
    } finally { if (p.current()) { controls.disabled = blocked; save.disabled = blocked || !changes.size; } }
  });
  p.root.replaceChildren(form);
  form.querySelector?.('input')?.focus({ preventScroll: true });
  p.syncActivity();
}

export function addNode(p) {
  p.editing = true;
  const form = el('form', { autocomplete: 'off', class: 'entity-editor' });
  const draft = p.trackDraft?.(form);
  const controls = el('fieldset', { 'data-protected-actions': true });
  const url = input('worker_url', '', { type: 'url', required: true, placeholder: 'https://node-address:9091' });
  const fingerprint = input('worker_fingerprint', '', { required: true, spellcheck: 'false', maxlength: '128' });
  const code = input('pairing_code', '', { type: 'password', required: true, maxlength: '128', autocomplete: 'new-password', 'data-sensitive': true });
  const feedback = el('div', { 'aria-live': 'polite' });
  const validation = formFeedback(form, feedback, key => ({ worker_url: url, worker_fingerprint: fingerprint, code })[key]);
  let blocked = false;
  let pairedName;
  controls.append(el('h2', {}, 'Add a compute node'), text('Start pairing on the new machine using the terminal client. Copy the HTTPS cluster address, public-key fingerprint and pairing code it prints.', 'muted'),
    field('Node address', url), field('Node fingerprint', fingerprint, 'Compare this with the fingerprint shown on the new machine.'), field('Pairing code', code));
  const save = el('button', { type: 'submit', class: 'primary' }, 'Pair node');
  form.append(feedback, controls, editorActions(save, p.close, p.api, draft));
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (blocked || controls.disabled || !p.current() || !p.api.canChange() || !form.reportValidity()) return;
    let address;
    try { address = new URL(url.value); } catch { validation.error(invalidField('Enter the HTTPS cluster address printed by the node.', url)); return; }
    if (address.protocol !== 'https:' || address.username || address.password || address.hash || address.search) { validation.error(invalidField('Use an HTTPS cluster address without credentials, query parameters or a fragment.', url)); return; }
    controls.disabled = true;
    const body = { worker_url: address.href, worker_fingerprint: fingerprint.value.trim(), code: code.value.trim() };
    code.value = '';
    feedback.replaceChildren(message('Pairing node and confirming its identity…'));
    try {
      const result = await p.api.request('/zzrouter/v1/cluster/pairing/accept', { method: 'POST', body, signal: p.scope.signal, timeoutMs: 60000 });
      if (!p.current()) return;
      blocked = true;
      if (!result.node_name) throw new Error('Pairing returned no node identity. Inspect current nodes before trying again.');
      pairedName = result.node_name;
      draft?.clean();
      feedback.replaceChildren(message(`Paired with ${result.node_name}. Checking the coordinator’s node observations…`));
      const known = await p.api.list('/nodes', { signal: p.scope.signal });
      if (!p.current()) return;
      const node = known.find(node => node.name === result.node_name);
      feedback.replaceChildren(message(node?.health_status === 'healthy' ? `${result.node_name} is paired and healthy.` : `${result.node_name} is paired. Healthy node observation is still pending.`, node?.health_status === 'healthy' ? '' : 'warning'),
        leaveButton('View node', () => p.reloadNode ? p.reloadNode(pairedName) : p.go('nodes', nodeKey(pairedName)), p.api, 'primary'));
    } catch (error) {
      if (!p.current()) return;
      blocked = true;
      validation.error(error);
      feedback.append(message(pairedName ? `${pairedName} is paired. Current node observations are unavailable. Refresh Nodes to check health.` : 'Pairing was not confirmed. Enrollment may already have completed. Check the node inventory before trying again.', 'warning'), leaveButton('Inspect nodes', () => p.go('nodes'), p.api));
    } finally { clearSecrets(form); if (p.current()) { controls.disabled = blocked; save.disabled = blocked; } }
  });
  p.root.append(form);
  p.scope.signal.addEventListener('abort', () => clearSecrets(form), { once: true });
  p.syncActivity();
}
