import { Lifetime } from './api.mjs';
import { el, text, button, actions, field, input, message, problem, facts } from './dom.mjs';
import { ownedPath } from './workload-data.mjs';
import { appendLog, readEvents } from './event-stream.mjs';

export async function runLogs(p, { node, id, back }) {
  const reads = new Lifetime(), feedback = el('div', { 'aria-live': 'polite' });
  const output = el('pre', { class: 'log-output', tabindex: '0', role: 'region', 'aria-label': 'Runtime output', 'data-sensitive': true });
  const follow = input('follow_output', '', { type: 'checkbox', checked: true });
  const lines = [];
  let frame;
  const stop = button('Pause', () => { reads.end(); stop.disabled = true; feedback.replaceChildren(message('Stream paused. Reconnect replaces the displayed replay with current output.')); });
  stop.disabled = true;
  const paint = () => {
    if (frame || !p.current()) return;
    frame = requestAnimationFrame(() => {
      frame = undefined;
      if (!p.current()) return;
      output.textContent = lines.join('\n');
      if (follow.checked) output.scrollTop = output.scrollHeight;
    });
  };
  p.scope.signal.addEventListener('abort', () => { reads.end(); cancelAnimationFrame(frame); lines.length = 0; output.textContent = ''; }, { once: true });
  p.root.append(el('h2', {}, 'Runtime logs'), facts([['Node', node], ['Run ID', id]]),
    actions(button('Read latest', tail), button('Follow live', stream), stop, back ? button('Back to run', back) : null),
    field('Follow output', follow), feedback, output,
    text('Shows up to 1,000 recent lines and 1 MiB. Reconnecting replays the file and replaces this buffer. Output may contain sensitive data.', 'muted'));
  const path = ownedPath('runs', node, id).replace('?', '/logs?');
  async function tail() {
    const scope = reads.next(); const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    stop.disabled = true; feedback.replaceChildren(message('Reading recent output…'));
    try {
      const result = await p.api.data(path + '&lines=250', { signal, timeoutMs: 45000 });
      if (!p.current() || !scope.current()) return;
      lines.length = 0;
      for (const line of result.lines || []) appendLog(lines, line);
      paint(); feedback.replaceChildren(text(result.truncated ? 'The server returned a truncated tail.' : `${lines.length} recent lines.`, 'muted'));
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error)); }
  }
  async function stream() {
    const scope = reads.next(); const signal = AbortSignal.any([p.scope.signal, scope.signal]);
    stop.disabled = false; feedback.replaceChildren(message('Connecting to runtime output…'));
    try {
      const response = await p.api.stream(path + '&follow=true', { signal });
      if (!p.current() || !scope.current()) { await response.body.cancel(); return; }
      lines.length = 0; paint(); feedback.replaceChildren(text('Live output. The file is replayed from its beginning; only the bounded recent tail is retained.', 'muted'));
      await readEvents(response, event => {
        if (!p.current() || !scope.current()) return false;
        if (event.event === 'error') throw new Error(event.data);
        if (event.event === 'log') { appendLog(lines, event.data); paint(); }
        return !['done', 'stream_closed'].includes(event.event);
      }, signal);
      if (p.current() && scope.current()) feedback.replaceChildren(message('The log stream ended. Read the run status separately; reconnect to follow again.'));
    } catch (error) { if (p.current() && scope.current()) feedback.replaceChildren(problem(error)); }
    finally { if (p.current() && scope.current()) stop.disabled = true; }
  }
  follow.addEventListener('change', () => { if (follow.checked) output.scrollTop = output.scrollHeight; });
  await tail();
}
