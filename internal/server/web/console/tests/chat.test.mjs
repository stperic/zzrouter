import test from 'node:test';
import assert from 'node:assert/strict';
import { API } from '../api.mjs';
import { chatModels, defaultSettings } from '../chat.mjs';
import { createChatAdapter } from '../vendor/chat-transport.mjs';

const encoder = new TextEncoder();
const frame = value => `data: ${JSON.stringify(value)}\n\n`;
const delta = (value, finish = null) => ({ choices: [{ delta: value, finish_reason: finish }] });
const user = { role: 'user', content: [{ type: 'text', text: 'Hello' }] };
function response(source, fragment = false) {
  const bytes = encoder.encode(source);
  return new Response(new ReadableStream({ start(controller) {
    for (let i = 0; i < bytes.length; i += fragment ? 3 : bytes.length) controller.enqueue(bytes.slice(i, i + (fragment ? 3 : bytes.length)));
    controller.close();
  } }), { headers: { 'Content-Type': 'text/event-stream', 'X-zzrouter-Node': 'worker', 'X-zzrouter-Provider': 'vllm' } });
}
async function collect(adapter, signal = new AbortController().signal) {
  const values = [];
  for await (const value of adapter.run({ messages: [user], abortSignal: signal })) values.push(value);
  return values;
}

test('chat discovery retains route aliases and unknown capability evidence', () => {
  assert.deepEqual(chatModels({ data: [{ id: 'embed', capabilities: { chat: false } }, { id: 'coding', capabilities: { chat: true } }, { id: 'cloud/model' }] }).map(x => x.id), ['cloud/model', 'coding']);
  assert.throws(() => chatModels({ data: null }));
  assert.equal(defaultSettings().model, '', 'sending requires an explicit choice');
});

test('real SDK preserves fragmented Unicode, loading events, reasoning aliases and late usage', async () => {
  let request;
  const statuses = [];
  const adapter = createChatAdapter({ baseURL: 'https://router.example/v1', settings: () => ({ model: 'coding', node: 'worker', system: 'Be concise.', temperature: 0, topP: 0.7, maxTokens: 64 }), signal: new AbortController().signal,
    onStatus: status => statuses.push(status), fetcher: async (url, init) => {
      request = { url, body: JSON.parse(init.body), headers: new Headers(init.headers) };
      return response('event: status_update\ndata: {"status":"loading_model","message":"Loading model"}\n\n' +
        frame(delta({ reasoning_content: 'Think ' })) + frame(delta({ reasoning: 'carefully.' })) + frame(delta({ content: 'Hello 🌍' })) +
        frame(delta({}, 'stop')) + frame({ choices: [], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15, cost: 0.001, prompt_tokens_details: { cached_tokens: 2 }, completion_tokens_details: { reasoning_tokens: 3 } } }) + 'data: [DONE]\n\n', true);
    } });
  const values = await collect(adapter);
  const last = values.at(-1);
  assert.deepEqual(last.content, [{ type: 'reasoning', text: 'Think carefully.' }, { type: 'text', text: 'Hello 🌍' }]);
  assert.equal(last.metadata.custom.inputTokens, 10);
  assert.equal(last.metadata.custom.outputTokens, 5);
  assert.equal(last.metadata.custom.cost, 0.001);
  assert.equal(last.metadata.custom.node, 'worker');
  assert.equal(last.metadata.custom.provider, 'vllm');
  assert.equal(last.metadata.custom.cachedTokens, 2);
  assert.equal(last.metadata.custom.reasoningTokens, 3);
  assert.ok(statuses.includes('Loading model'));
  assert.equal(request.url, 'https://router.example/v1/chat/completions');
  assert.equal(request.headers.get('X-Node'), 'worker');
  assert.equal(request.headers.get('Authorization'), null);
  assert.deepEqual(request.body.messages, [{ role: 'system', content: 'Be concise.' }, { role: 'user', content: 'Hello' }]);
  assert.equal(request.body.temperature, 0);
  assert.equal(request.body.top_p, 0.7);
  assert.equal(request.body.max_tokens, 64);
  assert.deepEqual(request.body.stream_options, { include_usage: true });
});

test('real SDK preserves multiline SSE data with CRLF and split frames', async () => {
  const multiline = value => JSON.stringify(value, null, 2).split('\n').map(line => `data: ${line}\r\n`).join('') + '\r\n';
  const source = ': keepalive\r\n' + multiline(delta({ content: 'First line\nSecond line 🌍' })) +
    multiline(delta({}, 'stop')) + multiline({ choices: [], usage: { prompt_tokens: 4, completion_tokens: 8, total_tokens: 12 } }) + 'data: [DONE]\r\n\r\n';
  const adapter = createChatAdapter({ baseURL: 'https://router.example/v1', settings: () => ({ model: 'alias' }), signal: new AbortController().signal,
    fetcher: async () => response(source, true) });
  const last = (await collect(adapter)).at(-1);
  assert.deepEqual(last.content, [{ type: 'text', text: 'First line\nSecond line 🌍' }]);
  assert.equal(last.status.type, 'complete');
  assert.equal(last.metadata.custom.totalTokens, 12);
});

test('defaults are omitted and DONE alone terminates a valid response', async () => {
  let body;
  const adapter = createChatAdapter({ baseURL: 'https://router.example/v1', settings: () => ({ model: 'alias' }), signal: new AbortController().signal, fetcher: async (_, init) => {
    body = JSON.parse(init.body); return response(frame(delta({ content: 'Done' })) + 'data: [DONE]\n\n');
  } });
  const values = await collect(adapter);
  assert.equal(values.at(-1).content[0].text, 'Done');
  assert.equal(values.at(-1).status.type, 'complete');
  for (const key of ['temperature', 'top_p', 'max_tokens']) assert.equal(key in body, false);
  assert.equal(body.model, 'alias');
});

test('token-limit, filtered and tool-call finishes are visibly incomplete', async () => {
  for (const [finish, reason] of [['length', 'length'], ['content_filter', 'content-filter'], ['tool_calls', 'tool-calls']]) {
    const adapter = createChatAdapter({ baseURL: 'https://router.example/v1', settings: () => ({ model: 'alias' }), signal: new AbortController().signal,
      fetcher: async () => response(frame(delta({ content: 'Partial answer' })) + frame(delta({}, finish)) + 'data: [DONE]\n\n') });
    const values = await collect(adapter);
    assert.deepEqual(values.at(-1).status, { type: 'incomplete', reason });
  }
});

test('cut streams and SSE errors preserve partial output and are not retried', async () => {
  for (const suffix of ['', frame({ error: { message: 'Budget refused', type: 'quota_exceeded' } }), 'event: status_update\ndata: {"status":"error","message":"Could not load model"}\n\n']) {
    let calls = 0;
    const adapter = createChatAdapter({ baseURL: 'https://router.example/v1', settings: () => ({ model: 'alias' }), signal: new AbortController().signal, fetcher: async () => { calls++; return response(frame(delta({ content: 'Partial' })) + suffix); } });
    const values = await collect(adapter);
    assert.equal(values.at(-1).content[0].text, 'Partial');
    assert.equal(values.at(-1).status.reason, 'error');
    assert.equal(calls, 1);
  }
});

test('Stop cancels the actual SDK request without an error or retry', async () => {
  let requestSignal;
  let calls = 0;
  const controller = new AbortController();
  const adapter = createChatAdapter({ baseURL: 'https://router.example/v1', settings: () => ({ model: 'alias' }), signal: new AbortController().signal, fetcher: async (_, init) => {
    calls++; requestSignal = init.signal;
    return new Response(new ReadableStream({ start(stream) {
      stream.enqueue(encoder.encode(frame(delta({ content: 'Starting' }))));
      init.signal.addEventListener('abort', () => stream.error(init.signal.reason), { once: true });
    } }), { headers: { 'Content-Type': 'text/event-stream' } });
  } });
  const values = [];
  for await (const value of adapter.run({ messages: [user], abortSignal: controller.signal })) { values.push(value); controller.abort(); }
  assert.equal(requestSignal.aborted, true);
  assert.equal(calls, 1);
  assert.ok(values.every(value => !value.status));
});

test('console streaming uses bound session, preserves policy refusals and aborts on identity change', async () => {
  let options, rejected = 0;
  const api = new API({ origin: 'https://router.example', rejected: () => rejected++, fetcher: async (_, init) => {
    options = init;
    return response(frame(delta({}, 'stop')));
  } });
  api.state = { csrf: 'proof' };
  const init = { method: 'POST', body: '{}', headers: { 'X-Node': 'worker', Authorization: 'must-not-forward' } };
  await api.inferenceFetch('/v1/chat/completions', init);
  assert.equal(options.headers['X-CSRF-Token'], 'proof');
  assert.equal(options.headers['X-Node'], 'worker');
  assert.equal(options.headers.Authorization, undefined);
  assert.equal(options.credentials, 'same-origin');
  assert.equal(options.redirect, 'error');
  assert.equal(api.canLeave(), true);
  api.clear();
  assert.equal(options.signal.aborted, true);
  assert.equal(api.unknownChange, false);
  api.state = { csrf: 'new-proof' };
  api.fetcher = async () => new Response(JSON.stringify({ error: { message: 'Provider access denied', code: 'model_access_denied' } }), { status: 403 });
  await assert.rejects(api.inferenceFetch('/v1/chat/completions', init), error => error.code === 'model_access_denied');
  assert.equal(rejected, 0);
  api.fetcher = async () => new Response(JSON.stringify({ error: { message: 'Expired' } }), { status: 401 });
  await assert.rejects(api.inferenceFetch('/v1/chat/completions', init));
  assert.equal(rejected, 1);
  assert.throws(() => api.inferenceFetch('https://other.example/v1/chat/completions', init));
});
