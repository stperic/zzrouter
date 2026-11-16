import test from 'node:test';
import assert from 'node:assert/strict';
import { API } from '../api.mjs';
import { appendLog, eventJSON, readEvents } from '../event-stream.mjs';
import { logMatches, logSummary } from '../logs.mjs';
import { versionText } from '../updates.mjs';

test('management raw transport accepts only read-only job and runtime streams', async () => {
  const calls = [];
  const api = new API({ origin: 'https://router.example', fetcher: async (path, options) => {
    calls.push({ path, options }); return new Response('event: done\ndata: {}\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
  } });
  api.state = { csrf: 'proof' };
  for (const path of ['/jobs/job-one/stream', '/runs/run-one/logs?node=worker&follow=true']) {
    const result = await api.stream(path); assert.ok(result.body); await result.body.cancel();
  }
  assert.equal(calls.length, 2);
  assert.ok(calls.every(call => call.options.method === 'GET' && call.options.credentials === 'same-origin'));
  await assert.rejects(api.stream('/keys'), /Unsupported console request/);
  await assert.rejects(api.request('/zzrouter/v1/jobs/job-one/stream', { method: 'DELETE', raw: true }), /Unsupported console request/);
  assert.equal(calls.length, 2);
  assert.equal(api.pending.size, 0);
});

test('stream content type is checked, and browser authority cancels the underlying body', async () => {
  const bad = new API({ origin: 'https://router.example', fetcher: async () => new Response('<html>sign in</html>', { headers: { 'Content-Type': 'text/html' } }) });
  await assert.rejects(bad.stream('/jobs/id/stream'), /event stream/);
  let signal;
  const api = new API({ origin: 'https://router.example', fetcher: async (_, options) => {
    signal = options.signal; return new Response('event: done\ndata: {}\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
  } });
  const response = await api.stream('/jobs/id/stream'); api.clear();
  assert.equal(signal.aborted, true); await response.body.cancel();
});

test('merge-patch uses the existing CSRF mutation transport', async () => {
  let sent;
  const api = new API({ origin: 'https://router.example', fetcher: async (path, options) => {
    sent = { path, options }; return Response.json({ applied: true });
  } });
  api.state = { csrf: 'proof' };
  const patch = { defaults: { parameters: { temperature: null } } };
  await api.request('/zzrouter/v1/providers/ollama/parameters', { method: 'PATCH', body: patch, contentType: 'application/merge-patch+json' });
  assert.equal(new Headers(sent.options.headers).get('Content-Type'), 'application/merge-patch+json');
  assert.equal(new Headers(sent.options.headers).get('X-CSRF-Token'), 'proof');
  assert.deepEqual(JSON.parse(sent.options.body), patch);
  assert.equal(api.pending.size, 0);
});

test('incremental SSE preserves split UTF-8, multiline fields and explicit terminal events', async () => {
  const bytes = new TextEncoder().encode(': heartbeat\r\nevent: log\r\ndata: héllo\r\ndata: second line\r\n\r\nevent: done\ndata: {}\n\n');
  const body = new ReadableStream({ start(controller) { for (const byte of bytes) controller.enqueue(Uint8Array.of(byte)); controller.close(); } });
  const events = [];
  await readEvents(new Response(body), event => events.push(event), new AbortController().signal);
  assert.deepEqual(events.map(event => [event.event, event.data]), [['log', 'héllo\nsecond line'], ['done', '{}']]);
  assert.throws(() => eventJSON({ data: '{broken' }), /unreadable event/);
});

test('an aborted event stream releases its body and never emits a completion', async () => {
  let cancelled = false;
  const abort = new AbortController();
  const body = new ReadableStream({ cancel() { cancelled = true; } });
  const task = readEvents(new Response(body), () => assert.fail('no event was sent'), abort.signal);
  abort.abort(); await assert.rejects(task, { name: 'AbortError' });
  await new Promise(resolve => setImmediate(resolve)); assert.equal(cancelled, true);
});

test('runtime buffers preserve repeated output, strip control codes and stay bounded with multibyte text', () => {
  const lines = [];
  appendLog(lines, '\x1b[31mready\x1b[0m'); appendLog(lines, 'ready'); assert.deepEqual(lines, ['ready', 'ready']);
  for (let i = 0; i < 1100; i++) appendLog(lines, '界'.repeat(16384));
  assert.ok(lines.length <= 1000); assert.ok(Buffer.byteLength(lines.join('\n')) <= 1024 * 1024);
});

test('live inference observations honor all active filters and retain no inline payloads', () => {
  const now = Date.parse('2026-11-15T12:00:00Z');
  const row = { id: 'one', model: 'model', status: 'success', key_id: 'key', team_id: 'team', timestamp: new Date(now - 60000).toISOString(), system_prompt: 'secret', messages: ['secret'], response: 'secret' };
  const filters = { model: 'MODEL', status: 'success', key_id: 'key', team_id: 'team', since: '5m' };
  assert.equal(logMatches(row, filters, now), true);
  assert.equal(logMatches({ ...row, timestamp: new Date(now - 600000).toISOString() }, filters, now), false);
  assert.equal(logMatches({ ...row, team_id: 'other' }, filters, now), false);
  assert.equal(logMatches({ ...row, timestamp: 'bad' }, filters, now), false);
  assert.deepEqual(Object.keys(logSummary(row)).sort(), ['id', 'key_id', 'model', 'status', 'team_id', 'timestamp']);
});

test('update version presentation distinguishes missing observations and preserves build metadata', () => {
  assert.equal(versionText({ major: 0, minor: 2, patch: 3, pre_release: 'rc1', build_meta: '1115' }), '0.2.3-rc1+1115');
  assert.equal(versionText({ major: 0 }), 'Not reported');
  assert.equal(versionText(undefined), 'Not reported');
});
