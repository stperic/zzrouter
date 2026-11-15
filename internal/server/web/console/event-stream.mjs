import { EventSourceParserStream } from './vendor/events.mjs';

// Framing is shared; each endpoint owns its replay and completion semantics.
export async function readEvents(response, receive, signal) {
  const reader = response.body.pipeThrough(new TextDecoderStream())
    .pipeThrough(new EventSourceParserStream({ onError: 'terminate', maxBufferSize: 1024 * 1024 })).getReader();
  const abort = () => { void reader.cancel().catch(() => {}); };
  signal.addEventListener('abort', abort, { once: true });
  try {
    signal.throwIfAborted();
    while (true) {
      const { value, done } = await reader.read();
      signal.throwIfAborted();
      if (done || receive(value) === false) break;
    }
  } finally {
    signal.removeEventListener('abort', abort);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

export function eventJSON(event) {
  try { return JSON.parse(event.data); }
  catch { throw new Error('The coordinator returned an unreadable event. Reconnect to read current observations.'); }
}

export function appendLog(lines, value) {
  // Preserve repeated lines; text equality is not an event identity.
  const clean = String(value).replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '').replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, '');
  lines.push(clean.slice(0, 16384));
  if (lines.length > 1000) lines.splice(0, lines.length - 1000);
  // A conservative character budget also bounds multibyte UTF-8 text to 1 MiB.
  let size = lines.reduce((sum, line) => sum + line.length + 1, 0);
  while (size > 256 * 1024 && lines.length > 1) size -= lines.shift().length + 1;
}
