import './schema-config';
import { streamText, type ModelMessage } from 'ai';
import { createOpenAICompatible, type MetadataExtractor } from '@ai-sdk/openai-compatible';
import { EventSourceParserStream } from 'eventsource-parser/stream';
import type { ChatModelAdapter, ChatModelRunResult, MessageStatus, TextMessagePart, ReasoningMessagePart } from '@assistant-ui/react';

export type ChatSettings = {
  model: string;
  node?: string;
  system?: string;
  temperature?: number;
  topP?: number;
  maxTokens?: number;
  showReasoning?: boolean;
  showStats?: boolean;
};

export type ChatHost = {
  baseURL: string;
  fetcher: typeof fetch;
  settings: () => ChatSettings;
  signal: AbortSignal;
  onStatus?: (message: string) => void;
};

type ChatMetricsChunk = {
  choices?: { finish_reason?: string | null }[];
  timings?: Record<string, unknown>;
  metrics?: Record<string, unknown>;
  usage?: Record<string, unknown>;
  x_groq?: { usage?: Record<string, unknown> };
};

function reportedNumber(...values: unknown[]) {
  return values.find((value): value is number => typeof value === 'number' && Number.isFinite(value) && value >= 0);
}

function secondsToMilliseconds(value: unknown) {
  return typeof value === 'number' ? value * 1000 : undefined;
}

// Only terminal chunks carry request-wide stats. MLX-VLM also emits per-token rates.
const timingMetadata: MetadataExtractor = {
  async extractMetadata() { return undefined; },
  createStreamExtractor() {
    const stats: Record<string, number> = {};
    return {
      processChunk(chunk) {
        const frame = chunk as ChatMetricsChunk | null;
        if (!Array.isArray(frame?.choices) || (frame.choices.length > 0 && !frame.choices.some(choice => choice?.finish_reason))) return;
        const { timings, metrics, usage, x_groq } = frame;
        const groqUsage = x_groq?.usage;
        for (const [name, value] of Object.entries({
          promptMs: reportedNumber(timings?.prompt_ms,
            secondsToMilliseconds(usage?.prompt_time), secondsToMilliseconds(groqUsage?.prompt_time)),
          providerFirstTokenMs: reportedNumber(metrics?.time_to_first_token_ms),
          generationMs: reportedNumber(timings?.predicted_ms, metrics?.generation_time_ms,
            secondsToMilliseconds(usage?.completion_time), secondsToMilliseconds(groqUsage?.completion_time)),
          tokensPerSecond: reportedNumber(timings?.predicted_per_second, metrics?.tokens_per_second),
          inputTokens: reportedNumber(groqUsage?.prompt_tokens),
          outputTokens: reportedNumber(groqUsage?.completion_tokens),
          totalTokens: reportedNumber(groqUsage?.total_tokens),
        })) {
          if (value !== undefined) stats[name] = value;
        }
      },
      buildMetadata: () => ({ zzrouter: stats }),
    };
  },
};

// zzRouter adds loading events and accepts [DONE] without a finish_reason.
// The library parser owns SSE framing; this adapter preserves those extensions.
function compatibleStream(response: Response, onStatus: ChatHost['onStatus']) {
  let finished = false;
  const encode = new TextEncoder();
  const stream = response.body!
    .pipeThrough(new TextDecoderStream())
    .pipeThrough(new EventSourceParserStream({ onError: 'terminate', maxBufferSize: 1024 * 1024 }))
    .pipeThrough(new TransformStream({
      transform({ event, data }, controller) {
        if (!data.trim()) return;
        if (data === '[DONE]') {
          if (!finished) controller.enqueue(encode.encode('data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n'));
          finished = true;
        } else {
          let value;
          try { value = JSON.parse(data); }
          catch { throw new Error('The coordinator returned an invalid chat stream.'); }
          if (event === 'status_update') {
            const message = typeof value.message === 'string' ? value.message.slice(0, 1024) : 'Preparing model…';
            if (value.status === 'error') throw new Error(message);
            onStatus?.(message);
            return;
          }
          finished ||= Boolean(value.choices?.[0]?.finish_reason);
        }
        controller.enqueue(encode.encode(`data: ${data.replaceAll('\n', '\ndata: ')}\n\n`));
      },
    }));
  return new Response(stream, { status: response.status, headers: response.headers });
}

function errorText(error: unknown) {
  const detail = error as { message?: string; code?: string; request_id?: string };
  return [detail?.message || 'The chat request failed.', detail?.code, detail?.request_id && `Request ${detail.request_id}`].filter(Boolean).join(' · ').slice(0, 2048);
}

export function createChatAdapter(host: ChatHost): ChatModelAdapter {
  return {
    async *run({ messages, abortSignal }) {
      const settings = { ...host.settings() };
      const signal = AbortSignal.any([abortSignal, host.signal]);
      const started = performance.now();
      const content: (TextMessagePart | ReasoningMessagePart)[] = [];
      const stats: Record<string, unknown> = { model: settings.model };
      let status: MessageStatus | undefined;
      const snapshot = (): ChatModelRunResult => ({ content: [...content], status, metadata: { custom: { ...stats } } });
      try {
        if (!settings.model) throw new Error('Select a model or route before sending.');
        signal.throwIfAborted();
        host.onStatus?.('Waiting for the model…');
        const provider = createOpenAICompatible({
          name: 'zzrouter', baseURL: host.baseURL, includeUsage: true, metadataExtractor: timingMetadata,
          fetch: async (url, init) => {
            const response = await host.fetcher(url, init);
            stats.route = response.headers.get('X-zzrouter-Route');
            stats.node = response.headers.get('X-zzrouter-Node');
            stats.provider = response.headers.get('X-zzrouter-Provider');
            return compatibleStream(response, host.onStatus);
          },
        });
        const history: ModelMessage[] = messages.filter(message => message.role === 'user' || message.role === 'assistant').map(message => ({
          role: message.role as 'user' | 'assistant',
          content: message.content.filter(part => part.type === 'text').map(part => part.text).join(''),
        }));
        const result = streamText({
          model: provider.chatModel(settings.model), messages: history, system: settings.system || undefined,
          temperature: settings.temperature, topP: settings.topP, maxOutputTokens: settings.maxTokens,
          headers: settings.node ? { 'X-Node': settings.node } : undefined,
          abortSignal: signal, maxRetries: 0, onError: () => {},
        });
        for await (const part of result.stream) {
          signal.throwIfAborted();
          if (part.type === 'error') throw part.error;
          if (part.type === 'text-delta' || part.type === 'reasoning-delta') {
            if (part.text && stats.firstTokenMs === undefined) stats.firstTokenMs = performance.now() - started;
            host.onStatus?.('');
            const type = part.type === 'text-delta' ? 'text' : 'reasoning';
            const last = content.at(-1);
            if (last?.type === type) content[content.length - 1] = { ...last, text: last.text + part.text };
            else content.push({ type, text: part.text });
            yield snapshot();
          }
          if (part.type === 'finish-step') {
            status = part.finishReason === 'stop' ? { type: 'complete', reason: 'stop' } : { type: 'incomplete', reason: part.finishReason === 'interrupted' ? 'other' : part.finishReason };
            const reported = part.providerMetadata?.zzrouter;
            Object.assign(stats, {
              inputTokens: part.usage.inputTokens ?? reported?.inputTokens,
              outputTokens: part.usage.outputTokens ?? reported?.outputTokens,
              totalTokens: part.usage.totalTokens ?? reported?.totalTokens,
              cachedTokens: part.usage.inputTokenDetails.cacheReadTokens,
              reasoningTokens: part.usage.outputTokenDetails.reasoningTokens, cost: part.usage.raw?.zz_cost_usd ?? part.usage.raw?.cost,
              promptMs: reported?.promptMs, providerFirstTokenMs: reported?.providerFirstTokenMs,
              generationMs: reported?.generationMs, tokensPerSecond: reported?.tokensPerSecond,
              durationMs: performance.now() - started, finishReason: part.finishReason,
            });
            yield snapshot();
          }
        }
      } catch (error) {
        if (signal.aborted) return;
        yield { ...snapshot(), status: { type: 'incomplete', reason: 'error', error: errorText(error) } };
      } finally {
        if (!host.signal.aborted) host.onStatus?.('');
      }
    },
  };
}
