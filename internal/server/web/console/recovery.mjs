import { el, text, card, copyField, facts, message, problem } from './dom.mjs';

export async function recovery(p) {
  const root = card('Installation recovery', message('Reading recovery scope…'));
  p.root.append(root);
  try {
    const inventory = await p.api.data('/system/recovery', { signal: p.scope.signal });
    if (!p.current()) return;
    const quote = value => "'" + String(value).replaceAll("'", inventory.platform === 'windows' ? "''" : "'\"'\"'") + "'";
    const command = 'zzrouter-node recovery';
    const dir = ' --dir ' + quote(inventory.config_dir);
    root.replaceChildren(el('h2', {}, 'Installation recovery'),
      text('Back up each node locally while it is stopped. The bundle contains accounts, app keys, team policies, spend, cluster identity, provider configuration, protected connections and node data. Model weights, executable runtimes and external dependencies have separate recovery requirements.'),
      facts([['Node', inventory.node], ['Platform', inventory.platform], ['Release', inventory.release]]),
      el('section', { class: 'access-section' }, el('h3', {}, '1. Review the scope'), copyField('Inspect this installation', command + ' inventory' + dir),
        text('Run as the installation owner or an administrator. Stop the node and its managed providers, and finish other installation or configuration work before backing up.', 'muted'),
        el('details', {}, el('summary', {}, 'Separate recovery dependencies'), el('ul', {}, inventory.separate.map(item => el('li', {}, text(item.reason), item.path ? el('code', {}, item.path) : null))))),
      el('section', { class: 'access-section' }, el('h3', {}, '2. Back up and check'),
        text('Replace ARCHIVE and KEY_BACKUP with new paths in secure storage. Keep the key in a separate directory and backup location. The archive contains secrets; use encrypted storage.'),
        copyField('Create a backup', command + ' backup' + dir + ' --output ARCHIVE --key-output KEY_BACKUP'),
        copyField('Verify a backup', command + ' check ARCHIVE --key-file KEY_BACKUP')),
      el('section', { class: 'access-section' }, el('h3', {}, '3. Stage a recovery'),
        copyField('Restore into a new directory', command + ' restore ARCHIVE --key-file KEY_BACKUP --to NEW_STAGING_DIRECTORY'),
        text('Restore checks the bundle and writes into a new private directory. It never replaces a running installation. Review the manifest, restore ownership and paths, install the matching release and recover separate data before starting the node.', 'muted'),
        text('Verify sign-in, teams, app access, provider connections and one request after activation. An archive check alone does not prove that a restored installation can run.', 'muted')));
  } catch (error) { if (p.current()) root.replaceChildren(el('h2', {}, 'Installation recovery'), problem(error)); }
}
