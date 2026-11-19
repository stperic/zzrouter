import { el, text, button, leaveButton, actions, badge, facts, message, problem, date } from './dom.mjs';
import { liveReads } from './live-reads.mjs';
import { activityText, observeActivity } from './observations.mjs';
import { failureDetails } from './diagnostics.mjs';
import { parameterSettings } from './parameters.mjs';
import { runLogs } from './run-logs.mjs';
import { ownedPath, jobProgress, jobSubject, newerJob, workloadKey } from './workload-data.mjs';

export function runDetails(p, run) {
  let browser;
  try {
    const url = new URL(run.browser?.url, location.origin);
    if (run.browser?.url && ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password) browser = el('a', { href: url.href, target: '_blank', rel: 'noopener' }, `Open ${run.browser.name}`);
  } catch { /* Missing or malformed links are not actionable. */ }
  return el('div', {}, el('h2', {}, run.model || `${run.provider} service`), actions(badge(run.record_only ? 'Stopped record' : run.status), run.pinned ? badge('Pinned') : null),
    facts([['Node', run.node], ['Provider', run.provider], ['Run ID', run.id], ['Endpoint', run.endpoint || 'Not reported'], ['Started', run.started_at ? date(run.started_at) : 'Not reported'], ['Active requests', run.active_requests ?? 0]]),
    activityText(p, run.activity), run.record_only ? message('This is a retained pin without a running process. Removing it deletes the retained request.') : null,
    browser, run.waiting_on ? text(`Waiting for run ${run.waiting_on}`) : null,
    run.error_message ? message(run.error_message, 'warning') : null,
    run.last_refusal ? message(`${run.last_refusal.code}: ${run.last_refusal.message}`, 'warning') : null,
    failureDetails(run.failure),
    run.memory_commitments_mib ? el('section', {}, el('h3', {}, 'Memory commitments'), facts(Object.entries(run.memory_commitments_mib).map(([key, value]) => [key, `${value} MiB`]))) : null,
    run.cache ? el('section', {}, el('h3', {}, 'Reported cache'), facts(Object.entries(run.cache).map(([key, value]) => [key.replaceAll('_', ' '), value]))) : null,
    run.evicted_runs?.length ? el('section', {}, el('h3', {}, 'Runs stopped for memory'), run.evicted_runs.map(value => text(`${value.model} · ${value.run_id} · ${value.freed_mib} MiB`))) : null);
}

export function jobDetails(job) {
  return el('div', {}, el('h2', {}, jobSubject(job)), actions(badge(job.kind), badge(job.phase)),
    facts([['Node', job.node], ['Job ID', job.job_id], ['Latest event', date(job.at)], ['Retention deadline', job.expires_at ? date(job.expires_at) : 'Not reported']]),
    jobProgress(job), job.err ? message(job.err, 'error') : null, job.warning ? message(job.warning, 'warning') : null,
    text('A quiet progress stream does not establish failure. After a job completes, inspect its resource to confirm readiness.', 'muted'));
}

export async function inspectWorkload(p, kind, node, id, { controls = [], observed = () => {} } = {}) {
  const content = el('div'), feedback = el('div', { 'aria-live': 'polite' }), confirmation = el('div');
  let row, stopping = false, terminal = false, blocked = false;
  const pause = button('Pause updates', () => live.toggle());
  const refresh = leaveButton('Refresh', () => live.refresh(), p.api);
  const stop = button('Stop run', () => {
    if (blocked || !row || !p.api.canChange()) return;
    p.editing = true;
    confirmation.replaceChildren(message(`Stop ${row.model || row.provider} (${id}) on ${node}? Active work may be interrupted and any retained pin is removed.`, 'warning'),
      actions(button('Confirm stop', performStop, 'danger'), button('Cancel', () => { p.editing = false; confirmation.replaceChildren(); })));
  }, 'danger');
  stop.setAttribute('data-protected-actions', '');
  const cancel = button('Cancel job', async () => {
    if (blocked || terminal || !row || !p.api.canChange() || !confirm(`Request cancellation of job ${id} on ${node}? Work already completed will remain.`)) return;
    blocked = true; cancel.disabled = true;
    try {
      await live.settle();
      if (!p.current()) return;
      const result = await p.api.request('/zzrouter/v1' + ownedPath('jobs', node, id), { method: 'DELETE', signal: p.scope.signal, timeoutMs: 45000 });
      if (!p.current()) return;
      confirmation.replaceChildren(message(result.accepted ? 'Cancellation requested. The job state below is the current observation.' : 'The server returned a cancellation response. Inspect the current job state.'));
      await live.refresh();
    } catch (error) { if (p.current()) confirmation.replaceChildren(problem(error), leaveButton('Inspect current state', () => live.refresh(), p.api)); }
  }, 'danger');
  cancel.setAttribute('data-protected-actions', '');
  const openLogs = () => runLogs(p.panel.view('Run logs'), { node, id, back: () => inspectWorkload(p.panel.view('Run details'), kind, node, id) });
  p.root.append(actions(refresh, pause, controls), feedback, content, actions(...(kind === 'runs' ? [stop, leaveButton('Runtime logs', openLogs, p.api)] : [cancel])), confirmation);
  const idle = () => !terminal && !stopping && !p.editing && !content.contains(document.activeElement) && p.api.canChange();
  const live = liveReads(p, async automatic => {
    const startedAt = performance.now();
    try {
      const next = await p.api.data(ownedPath(kind, node, id), { signal: p.scope.signal, timeoutMs: 45000 });
      if (!p.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      row = kind === 'jobs' ? newerJob(row, { ...next, node }) : { ...next, node };
      observeActivity(row.activity, startedAt);
      content.replaceChildren(kind === 'jobs' ? jobDetails(row) : runDetails(p, row));
      const failedLaunch = kind === 'runs' ? row.status === 'failed' && row :
        row.kind === 'run' && row.phase === 'failed' && row.meta?.failure && row.meta;
      if (failedLaunch?.provider && failedLaunch.model) {
        const { provider, model } = failedLaunch;
        content.append(actions(leaveButton('Edit launch parameters', () => parameterSettings(p, {
          provider, model, node, back: () => inspectWorkload(p.panel.view(kind === 'runs' ? 'Run details' : 'Job details'), kind, node, id, { controls, observed }),
        }), p.api)));
      }
      terminal = kind === 'jobs' ? ['done', 'failed'].includes(row.phase) : row.record_only || ['stopped', 'failed', 'crashed'].includes(row.status);
      observed(row, terminal);
      stop.textContent = row.record_only ? 'Remove retained pin' : 'Stop run';
      stop.disabled = blocked;
      cancel.disabled = blocked || terminal || row.kind === 'inference_log';
      feedback.replaceChildren(blocked ? message('The action was submitted. Current observations are shown below; the request will not be repeated automatically.', 'warning') : text('Updates every 5 seconds while idle.', 'muted'));
      return true;
    } catch (error) {
      if (!p.current()) return false;
      if (automatic && !live.canUpdate()) return true;
      if (error.status === 404) {
        terminal = true; stop.disabled = true; cancel.disabled = true;
        observed(undefined, false);
        content.replaceChildren(message(kind === 'jobs' ? 'This job is missing or its retention period ended. Its outcome is unavailable.' : blocked ? 'This exact run is no longer listed on this node.' : 'This run is no longer listed. It may have stopped or been removed.', 'warning'));
      }
      feedback.replaceChildren(problem(error)); return false;
    }
  }, { eligible: idle, changed: () => { pause.textContent = live.paused ? 'Resume updates' : 'Pause updates'; pause.setAttribute('aria-pressed', String(live.paused)); } });
  async function performStop() {
    if (blocked || stopping || !p.current() || !p.api.canChange()) return;
    stopping = true; blocked = true; stop.disabled = true;
    confirmation.replaceChildren(message('Stopping this run and waiting for the node…'));
    try {
      await live.settle();
      if (!p.current()) return;
      await p.api.data(ownedPath('runs', node, id), { method: 'DELETE', signal: p.scope.signal, timeoutMs: 60000 });
      if (!p.current()) return;
      confirmation.replaceChildren(message('Stop acknowledged. Reading the exact run to verify its state…'));
      await live.refresh();
      if (p.current()) confirmation.replaceChildren(leaveButton('Inspect current state', () => live.refresh(), p.api));
    } catch (error) {
      if (p.current()) confirmation.replaceChildren(problem(error), leaveButton('Inspect current state', () => live.refresh(), p.api));
    } finally { stopping = false; if (p.current()) { p.editing = false; p.syncActivity(); } }
  }
  await live.refresh();
}

// A start result keeps its immutable operation identity. Only GETs run here.
export async function followStart(p, target, operation, result, error) {
  p.editing = false;
  const content = el('div'), feedback = el('div', { 'aria-live': 'polite' });
  const runID = result?.id || error?.run_id;
  const jobID = result?.job_id;
  let ready = false, terminal = false, job;
  const pause = button('Pause updates', () => live.toggle());
  const inspect = leaveButton('Inspect current state', () => live.refresh(), p.api);
  p.root.replaceChildren(el('h2', {}, 'Start result'),
    error ? problem(error) : message(`Start response: ${result?.status || 'accepted'}. Verifying current state.`),
    facts([['Node', target.node], ['Provider', target.provider], ['Model', target.model || 'Node service'], ['Operation ID', operation], ['Run ID', runID || 'Not returned']]),
    text('This operation will not be submitted again automatically.', 'muted'), actions(inspect, runID ? pause : null), feedback, content);
  const idle = () => !!runID && !ready && !terminal && !content.contains(document.activeElement) && p.api.canChange();
  const live = liveReads(p, async automatic => {
    const startedAt = performance.now();
    const reads = await Promise.allSettled([
      runID ? p.api.data(ownedPath('runs', target.node, runID), { signal: p.scope.signal, timeoutMs: 45000 }) : p.api.list(ownedPath('runs', target.node), { signal: p.scope.signal, timeoutMs: 45000 }),
      jobID ? p.api.data(ownedPath('jobs', target.node, jobID), { signal: p.scope.signal, timeoutMs: 45000 }) : Promise.resolve(null),
    ]);
    if (!p.current()) return false;
    if (automatic && !live.canUpdate()) return true;
    content.replaceChildren(); feedback.replaceChildren();
    if (reads[1].status === 'fulfilled' && reads[1].value) { job = newerJob(job, reads[1].value); content.append(jobDetails(job)); }
    else if (jobID) feedback.append(message('Job observation unavailable. The exact run is checked separately.', 'warning'), problem(reads[1].reason));
    if (reads[0].status === 'fulfilled') {
      if (runID) {
        const run = { ...reads[0].value, node: target.node };
        observeActivity(run.activity, startedAt);
        ready = run.status === 'running' && !run.record_only;
        terminal = run.record_only || ['stopped', 'failed', 'crashed'].includes(run.status);
        feedback.prepend(message(ready ? 'This run is reported running by its node.' : `Run status: ${run.status}.`, ready ? '' : 'warning'));
        content.prepend(runDetails(p, run), leaveButton('View run', () => p.go('workloads', workloadKey('run', target.node, runID), 'runs'), p.api));
      } else {
        feedback.append(message('No run identity was returned. These are current observations, not proof of this operation’s outcome. An empty list does not prove failure.', 'warning'));
        for (const run of reads[0].value.filter(row => row.provider === target.provider && (!target.model || row.model === target.model))) content.append(leaveButton(`${run.model || run.provider} · ${run.status} · ${run.id}`, () => p.go('workloads', workloadKey('run', target.node, run.id), 'runs'), p.api));
      }
    } else feedback.append(problem(reads[0].reason), message('Readiness is not confirmed. Inspect again to recover current state.', 'warning'));
    return reads.every(value => value.status === 'fulfilled');
  }, { eligible: idle, changed: () => { pause.textContent = live.paused ? 'Resume updates' : 'Pause updates'; } });
  await live.refresh();
}
