import { el, text, facts } from './dom.mjs';
import { observeActivity, gib } from './observations.mjs';

export const workloadKey = (kind, node, id) => `${kind}/${encodeURIComponent(node)}/${encodeURIComponent(id)}`;
export function workloadSelection(value = '') {
  const [kind, node, id] = value.split('/');
  try { return { kind, node: decodeURIComponent(node || ''), id: decodeURIComponent(id || '') }; }
  catch { return {}; }
}
export const ownedPath = (kind, node, id) => `/${kind}${id ? '/' + encodeURIComponent(id) : ''}?node=${encodeURIComponent(node)}`;

// Explicit node requests disclose failures that broadcast aggregation can omit.
export async function readWorkloadPages(p, nodes, kind, offsets = new Map()) {
  const pending = [...nodes], results = [];
  await Promise.all(Array.from({ length: Math.min(3, pending.length) }, async () => {
    while (pending.length && p.current()) {
      const node = pending.shift();
      const startedAt = performance.now();
      try {
        const response = await p.api.request('/zzrouter/v1' + ownedPath(kind, node) + (kind === 'jobs' ? `&limit=25&offset=${offsets.get(node) || 0}` : ''), { signal: p.scope.signal, timeoutMs: 45000 });
        if (!Array.isArray(response.data)) throw new Error('The server returned an invalid workload list.');
        const rows = response.data.map(value => ({ ...value, node }));
        for (const row of rows) observeActivity(row.activity, startedAt);
        results.push({ node, rows, more: response.has_more === true, offset: offsets.get(node) || 0 });
      } catch (error) { results.push({ node, error }); }
    }
  }));
  return results;
}

export function newerJob(previous, next) {
  if (!previous) return next;
  if (previous.epoch === next.epoch) return Number(next.seq) >= Number(previous.seq) ? next : previous;
  return Date.parse(next.at) >= Date.parse(previous.at) ? next : previous;
}
export function jobPercent(job) {
  if (Number.isFinite(job.percent)) return Math.max(0, Math.min(100, job.percent));
  if (Number.isFinite(job.bytes?.done) && job.bytes?.total > 0) return Math.max(0, Math.min(100, 100 * job.bytes.done / job.bytes.total));
  return undefined;
}
export const jobSubject = job => job.meta?.model || job.meta?.provider || job.meta?.name || job.job_id;
export function jobProgress(job) {
  const percent = jobPercent(job);
  return el('div', { class: 'job-progress' },
    el('progress', { max: 100, value: percent, 'aria-label': `${job.kind || 'Job'} progress` }),
    text(percent === undefined ? 'Progress not reported' : `${Math.round(percent)}%`, 'muted'),
    job.step ? text(job.step) : null,
    job.meta?.total_steps ? text(`Step ${job.meta.step ?? '?'} of ${job.meta.total_steps}`) : null,
    job.bytes?.total > 0 ? text(`${Number.isFinite(job.bytes.done) ? gib(job.bytes.done) : '?'} / ${gib(job.bytes.total)} GiB`) : null,
    job.meta?.transfer_status ? text(`Transfer: ${job.meta.transfer_status}`) : null,
    Number.isFinite(job.meta?.transfer_speed) ? text(`Speed: ${(job.meta.transfer_speed / 1048576).toLocaleString(undefined, { maximumFractionDigits: 1 })} MiB/s`) : null);
}

export function launchTargets(models, providers, node) {
  const capable = providers.filter(row => row.node === node && row.enabled && ['process', 'served'].includes(row.run_lifecycle));
  const targets = capable.filter(row => row.run_scope === 'node').map(row => ({ node, provider: row.key || row.name, label: `${row.name} service` }));
  for (const model of models) {
    if (model.node !== node || model.is_cloud || model.incomplete) continue;
    const provider = capable.find(row => (row.key || row.name) === model.assigned_app && row.run_scope !== 'node');
    if (provider) targets.push({ node, provider: provider.key || provider.name, model: model.name, label: `${model.name} · ${provider.name}` });
  }
  return targets;
}

export function runAssessment(preview) {
  const assessment = preview.assessment;
  return el('section', { class: 'access-section' }, el('h3', {}, 'API assessment'),
    !assessment ? text('No memory assessment was reported. This preview does not establish capacity or readiness.') : [
      facts([['Status', assessment.status], ['Reason', assessment.reason], ['Unknown cause', assessment.unknown_cause || 'None reported'],
        ['Required memory', assessment.required_mib ? `${assessment.required_mib} MiB` : 'Not reported'], ['Assessed capacity', assessment.capacity_mib ? `${assessment.capacity_mib} MiB` : 'Not reported']]),
    ], text('Preview is advisory. Start rechecks the target and current conditions. It can stop eligible idle runs under the node’s existing memory policy.', 'muted'));
}
