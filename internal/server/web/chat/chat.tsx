import './schema-config';
import { createContext, useContext, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  AssistantRuntimeProvider, useLocalRuntime, useAuiState, AuiIf,
  ThreadPrimitive, ComposerPrimitive, MessagePrimitive, ActionBarPrimitive,
  type AssistantRuntime, type ChatModelAdapter, type ReasoningMessagePartProps,
} from '@assistant-ui/react';
import { MarkdownTextPrimitive } from '@assistant-ui/react-markdown';
import { createChatAdapter, type ChatHost, type ChatSettings } from './transport';

const Settings = createContext<ChatSettings>({ model: '' });

function Markdown() {
  return <MarkdownTextPrimitive className="chat-markdown" components={{
    a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
    img: ({ alt }) => <span>{alt ? `[Image: ${alt}]` : '[Image]'}</span>,
  }} />;
}

function Reasoning({ text }: ReasoningMessagePartProps) {
  const { showReasoning } = useContext(Settings);
  return showReasoning ? <details className="chat-reasoning"><summary>Reasoning</summary><div>{text}</div></details> : null;
}

function MessageFooter() {
  const { showStats } = useContext(Settings);
  const status = useAuiState(state => state.message.status);
  const stats = useAuiState(state => state.message.metadata.custom);
  const entries = showStats ? [
    typeof stats.inputTokens === 'number' && `${stats.inputTokens.toLocaleString()} input tokens`,
    typeof stats.outputTokens === 'number' && `${stats.outputTokens.toLocaleString()} output tokens`,
    typeof stats.cachedTokens === 'number' && stats.cachedTokens > 0 && `${stats.cachedTokens.toLocaleString()} cached`,
    typeof stats.reasoningTokens === 'number' && stats.reasoningTokens > 0 && `${stats.reasoningTokens.toLocaleString()} reasoning`,
    typeof stats.durationMs === 'number' && `${(stats.durationMs / 1000).toFixed(1)}s`,
    typeof stats.cost === 'number' && `$${stats.cost.toFixed(6)}`,
    typeof stats.provider === 'string' && stats.provider,
    typeof stats.node === 'string' && stats.node,
  ].filter(Boolean) : [];
  return <>
    {status?.type === 'incomplete' && <p className={status.reason === 'error' ? 'notice error' : 'muted'} role="status">
      {status.reason === 'error' ? String(status.error || 'The response could not be completed.') : {
        cancelled: 'Response stopped.', length: 'Output-token limit reached.',
        'content-filter': 'The provider filtered this response.',
        'tool-calls': 'The model requested a tool. Tool execution is not available in this chat.',
        other: 'The model ended the response without reporting completion.',
      }[status.reason]}
    </p>}
    {entries.length > 0 && <p className="chat-stats">{entries.join(' · ')}</p>}
    <ActionBarPrimitive.Root className="chat-message-actions">
      <ActionBarPrimitive.Copy copiedDuration={1500}>
        <AuiIf condition={state => state.message.isCopied}>Copied</AuiIf>
        <AuiIf condition={state => !state.message.isCopied}>Copy</AuiIf>
      </ActionBarPrimitive.Copy>
      <span data-protected-actions><ActionBarPrimitive.Reload>Regenerate</ActionBarPrimitive.Reload></span>
    </ActionBarPrimitive.Root>
  </>;
}

function UserMessage() {
  return <MessagePrimitive.Root className="chat-message chat-user"><span className="chat-role">You</span><MessagePrimitive.Parts /></MessagePrimitive.Root>;
}

function AssistantMessage() {
  return <MessagePrimitive.Root className="chat-message chat-assistant"><span className="chat-role">Assistant</span>
    <MessagePrimitive.Parts components={{ Text: Markdown, Reasoning }} />
    <MessageFooter />
  </MessagePrimitive.Root>;
}

function Conversation({ status }: { status: string }) {
  const settings = useContext(Settings);
  return <ThreadPrimitive.Root className="chat-thread">
    <ThreadPrimitive.Viewport className="chat-viewport" autoScroll>
      <ThreadPrimitive.Empty><div className="chat-empty"><div className="chat-symbol" aria-hidden="true">✳</div>
        <h2>Talk to your models</h2><p>Choose a model or route, then start a conversation.</p>
        <p className="muted">Your key’s access rules and budgets apply.</p></div></ThreadPrimitive.Empty>
      <ThreadPrimitive.Messages components={{ UserMessage, AssistantMessage }} />
      <AuiIf condition={state => state.thread.isRunning}><p className="chat-progress" role="status"><span className="chat-dot" />{status || 'Generating…'}</p></AuiIf>
    </ThreadPrimitive.Viewport>
    <div className="chat-composer-area">
      <ThreadPrimitive.ScrollToBottom className="chat-scroll">Latest message ↓</ThreadPrimitive.ScrollToBottom>
      <ComposerPrimitive.Root className="chat-composer">
        <div data-protected-actions><ComposerPrimitive.Input aria-label="Message" placeholder={settings.model ? 'Message your model…' : 'Choose a model to start…'} rows={3} disabled={!settings.model} /></div>
        <div className="chat-composer-footer"><span>Enter to send · Shift+Enter for a new line</span>
          <span data-protected-actions><AuiIf condition={state => !state.thread.isRunning}><ComposerPrimitive.Send className="primary" disabled={!settings.model}>Send ↑</ComposerPrimitive.Send></AuiIf></span>
          <AuiIf condition={state => state.thread.isRunning}><ComposerPrimitive.Cancel>Stop ■</ComposerPrimitive.Cancel></AuiIf>
        </div>
      </ComposerPrimitive.Root>
      <p className="chat-privacy">Conversation stays in this view. Leaving or signing out clears it.</p>
    </div>
  </ThreadPrimitive.Root>;
}

function Chat({ adapter, settings, subscribeStatus, ready }: {
  adapter: ChatModelAdapter; settings: ChatSettings;
  subscribeStatus: (listener: (status: string) => void) => () => void;
  ready: (runtime: AssistantRuntime) => void;
}) {
  const runtime = useLocalRuntime(adapter);
  const [status, setStatus] = useState('');
  useEffect(() => subscribeStatus(setStatus), [subscribeStatus]);
  useEffect(() => { ready(runtime); return () => runtime.thread.cancelRun(); }, [runtime, ready]);
  return <Settings.Provider value={settings}><AssistantRuntimeProvider runtime={runtime}><Conversation status={status} /></AssistantRuntimeProvider></Settings.Provider>;
}

export function mountChat(element: HTMLElement, host: Omit<ChatHost, 'settings' | 'onStatus'> & { settings: ChatSettings }) {
  let settings = host.settings;
  let runtime: AssistantRuntime | undefined;
  let statusListener: ((status: string) => void) | undefined;
  const subscribeStatus = (listener: (status: string) => void) => { statusListener = listener; return () => { statusListener = undefined; }; };
  const ready = (value: AssistantRuntime) => { runtime = value; };
  const adapter = createChatAdapter({ ...host, settings: () => settings, onStatus: value => statusListener?.(value) });
  const root = createRoot(element);
  const render = () => root.render(<Chat adapter={adapter} settings={settings} subscribeStatus={subscribeStatus} ready={ready} />);
  render();
  host.signal.addEventListener('abort', () => { runtime?.thread.cancelRun(); root.unmount(); }, { once: true });
  return {
    configure(value: ChatSettings) { settings = value; render(); },
    reset() { runtime?.thread.cancelRun(); runtime?.thread.reset(); },
    get running() { return runtime?.thread.getState().isRunning || false; },
    get hasMessages() { return Boolean(runtime?.thread.getState().messages.length); },
  };
}
