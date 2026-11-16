import { el, text, facts, date } from './dom.mjs';

const deadlines = new WeakMap();
export function observeActivity(activity, startedAt = performance.now()) {
  if (activity && typeof activity === 'object') deadlines.set(activity, startedAt + Math.max(0, Number(activity.valid_for_seconds) || 0) * 1000);
  return activity;
}
export const activityDeadline = activity => activity && deadlines.get(activity) || 0;
export function activityLabel(activity, now = performance.now()) {
  if (!activity || activity.stale || activityDeadline(activity) <= now) return 'Activity unavailable';
  return { active: 'Active at observation', idle: 'No observed work' }[activity.state] || 'Activity unavailable';
}
export function activityText(p, activity) {
  const node = text(activityLabel(activity), 'muted activity-label');
  const remaining = activityDeadline(activity) - performance.now();
  if (remaining <= 0 || p.scope.signal.aborted) return node;
  const cancel = () => clearTimeout(timer);
  const timer = setTimeout(() => {
    p.scope.signal.removeEventListener('abort', cancel);
    if (p.current()) node.textContent = activityLabel(activity);
  }, remaining + 1);
  timer.unref?.();
  p.scope.signal.addEventListener('abort', cancel, { once: true });
  return node;
}

export const gib = bytes => (bytes / 1073741824).toLocaleString(undefined, { maximumFractionDigits: 1 });
export function memoryRows(node) {
  const row = (label, sample, totalGB, identity) => ({ label, sample, identity,
    value: `${sample && Number.isFinite(sample.used_bytes) ? gib(sample.used_bytes) : 'Unavailable'} / ${sample?.total_bytes > 0 ? gib(sample.total_bytes) : totalGB > 0 ? Number(totalGB).toLocaleString(undefined, { maximumFractionDigits: 1 }) : '?'} GiB`,
    stale: node.health_status !== 'healthy' || sample?.stale === true });
  const rows = [row(node.gpu?.unified_memory ? 'Unified RAM' : 'RAM', node.memory?.sample, node.memory?.total_gb)];
  if (!node.gpu?.unified_memory) for (const card of node.gpu?.gpus || []) rows.push(row(`GPU ${card.driver_index ?? card.index ?? rows.length - 1}`, card.sample, card.memory_total_gb, [card.name, card.uuid || card.pci_address].filter(Boolean).join(' · ')));
  return rows;
}
export function memorySummary(node) {
  const rows = memoryRows(node).filter((row, index) => index === 0 || Number.isFinite(row.sample?.used_bytes) || row.sample?.total_bytes > 0 || node.gpu?.gpus?.[index - 1]?.memory_total_gb > 0);
  return [...rows.slice(0, 3).map(row => `${row.label}  ${row.value}${row.stale ? ' *' : ''}`), rows.length > 3 ? `+ ${rows.length - 3} GPUs in details` : null].filter(Boolean).join('\n');
}
export function memoryDetails(node) {
  return el('section', { class: 'memory-details' }, el('h3', {}, 'Memory used / total'), memoryRows(node).map(row => el('div', {},
    facts([[row.label, row.value]]), row.identity ? text(row.identity, 'muted') : null,
    text(row.sample ? `${row.stale ? 'Last reported' : 'Observed'} ${date(row.sample.observed_at)} · ${Math.floor(row.sample.age_seconds || 0)} seconds old at refresh` : 'Measured usage has not been reported.', 'muted'))));
}
