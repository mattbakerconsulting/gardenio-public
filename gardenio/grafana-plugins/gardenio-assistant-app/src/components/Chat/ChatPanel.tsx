import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { css } from '@emotion/css';
import { GrafanaTheme2, renderMarkdown } from '@grafana/data';
import { llm, mcp } from '@grafana/llm';
import { Alert, Badge, Button, Icon, Spinner, TextArea, useStyles2 } from '@grafana/ui';
import { useAsync } from 'react-use';

import { testIds } from '../testIds';
import { curateTools } from '../../tools/readOnlyTools';
import { runChatTurn, type RenderedToolCall } from './chatEngine';

interface ChatTurn {
  role: 'user' | 'assistant';
  content: string;
  toolCalls?: RenderedToolCall[];
}

interface ChatPanelProps {
  /**
   * Per-env opt-in for dashboard/folder writes (plugin jsonData
   * `enableDashboardWrites`, default false). When true the curated surface adds
   * the vetted write tools and the system prompt advertises create/update; when
   * false the panel stays hard read-only (C8, ka-0000005589).
   */
  enableDashboardWrites?: boolean;
}

const SUGGESTIONS = [
  'Which dashboards mention cost?',
  'Show me CPU usage over the last hour.',
  'What datasources are configured here?',
];

/**
 * The gardenio-branded chat surface. Assumes it is rendered inside an
 * `mcp.MCPClientProvider` (see ChatPage), so `useMCPClient` resolves to a
 * connected client. Streams answers from grafana-llm-app and grounds them in
 * gardenio observability via curated mcp-grafana tools (read-only by default,
 * plus vetted dashboard/folder write tools when `enableDashboardWrites`).
 */
export function ChatPanel({ enableDashboardWrites = false }: ChatPanelProps) {
  const styles = useStyles2(getStyles);
  const { client, enabled: mcpEnabled } = mcp.useMCPClient();

  const [turns, setTurns] = useState<ChatTurn[]>([]);
  const [input, setInput] = useState('');
  const [isGenerating, setIsGenerating] = useState(false);
  const [sendError, setSendError] = useState<string | undefined>();

  const historyRef = useRef<HTMLDivElement>(null);
  // Aborts the in-flight turn when the component unmounts, so streaming
  // callbacks don't fire setState on an unmounted component.
  const abortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    return () => abortRef.current?.abort();
  }, []);

  // Discover the LLM + tool surface once. `llm.enabled()` confirms the LLM
  // provider is configured; the curated list is the surface we expose — read-only
  // tools always, plus the vetted dashboard/folder write tools when this env has
  // opted in via `enableDashboardWrites`.
  const {
    loading: toolsLoading,
    error: toolsError,
    value: toolsData,
  } = useAsync(async () => {
    const llmEnabled = await llm.enabled();
    if (!llmEnabled || !mcpEnabled || !client) {
      return { llmEnabled, tools: [] };
    }
    const { tools } = (await client.listTools()) ?? { tools: [] };
    return { llmEnabled, tools: curateTools(tools, enableDashboardWrites) };
  }, [client, mcpEnabled, enableDashboardWrites]);

  const tools = useMemo(() => toolsData?.tools ?? [], [toolsData]);

  // Auto-scroll to the newest message.
  useEffect(() => {
    if (historyRef.current) {
      historyRef.current.scrollTop = historyRef.current.scrollHeight;
    }
  }, [turns]);

  // Stop an in-flight generation: abort the stream (tears down the Live channel
  // and tells runChatTurn's content callbacks to stop firing) and re-enable the
  // composer. runChatTurn's own `finally` skips re-enabling once aborted, so the
  // reset must happen here. Idempotent — safe if no turn is in flight.
  const stopGenerating = useCallback(() => {
    abortRef.current?.abort();
    setIsGenerating(false);
  }, []);

  const sendMessage = useCallback(
    async (text: string) => {
      const trimmed = text.trim();
      if (!trimmed || isGenerating || !client || !toolsData?.llmEnabled) {
        return;
      }
      setSendError(undefined);
      setInput('');
      setIsGenerating(true);

      const controller = new AbortController();
      abortRef.current = controller;

      // Snapshot the history (user/assistant turns) BEFORE appending this turn.
      const priorHistory = turns.map((t) => ({ role: t.role, content: t.content }));

      // Append the user turn + an empty assistant placeholder.
      setTurns((prev) => [
        ...prev,
        { role: 'user', content: trimmed },
        { role: 'assistant', content: '', toolCalls: [] },
      ]);

      const updateAssistant = (patch: (turn: ChatTurn) => ChatTurn) => {
        if (controller.signal.aborted) {
          return;
        }
        setTurns((prev) => {
          const next = [...prev];
          for (let i = next.length - 1; i >= 0; i--) {
            if (next[i].role === 'assistant') {
              next[i] = patch(next[i]);
              break;
            }
          }
          return next;
        });
      };

      try {
        await runChatTurn({
          client,
          tools,
          enableWrites: enableDashboardWrites,
          history: priorHistory,
          userMessage: trimmed,
          signal: controller.signal,
          onAssistantContent: (content) => updateAssistant((t) => ({ ...t, content })),
          onToolCalls: (calls) => updateAssistant((t) => ({ ...t, toolCalls: calls })),
        });
      } catch (e) {
        if (controller.signal.aborted) {
          return;
        }
        const message = e instanceof Error ? e.message : 'Unknown error';
        setSendError(message);
        updateAssistant((t) => ({
          ...t,
          content: t.content || `Sorry — I hit an error: ${message}`,
        }));
      } finally {
        if (!controller.signal.aborted) {
          setIsGenerating(false);
        }
      }
    },
    [client, tools, turns, isGenerating, toolsData, enableDashboardWrites]
  );

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      sendMessage(input);
    }
  };

  if (toolsError) {
    return (
      <Alert title="Failed to initialize the assistant" severity="error">
        {toolsError.message}
      </Alert>
    );
  }

  const llmReady = toolsData?.llmEnabled ?? false;
  // LLM is up but no curated tools survived discovery: answers will not be
  // grounded in this Grafana's data. Surface the reduced fidelity (item 11).
  const groundingUnavailable = llmReady && !toolsLoading && tools.length === 0;

  return (
    <div className={styles.container} data-testid={testIds.chatPanel.container}>
      <header className={styles.header}>
        <div className={styles.brand}>
          <Icon name="message" size="lg" />
          <span className={styles.title}>Gardenio Assistant</span>
          {enableDashboardWrites ? (
            <Badge
              text="read + dashboard write"
              color="orange"
              icon="pen"
              tooltip="Can read observability data and create/update dashboards and folders only."
            />
          ) : (
            <Badge text="read-only" color="blue" icon="lock" />
          )}
        </div>
        <div className={styles.toolBadge}>
          {toolsLoading ? (
            <Spinner size="sm" />
          ) : (
            <Badge text={`${tools.length} tools`} color="green" icon="apps" tooltip={tools.map((t) => t.name).join(', ')} />
          )}
        </div>
      </header>

      {!llmReady && !toolsLoading && (
        <Alert title="LLM provider not configured" severity="warning">
          The Grafana LLM app is not enabled or configured in this instance. Ask an admin to enable
          grafana-llm-app and point it at the in-cluster LiteLLM backend.
        </Alert>
      )}

      {groundingUnavailable && (
        <Alert title="Grounding tools unavailable" severity="warning" data-testid={testIds.chatPanel.noTools}>
          No read-only mcp-grafana tools are available, so answers cannot be grounded in this
          Grafana&apos;s dashboards, metrics, or logs. Ensure grafana-llm-app&apos;s MCP server is
          enabled. Replies will be ungrounded until tools load.
        </Alert>
      )}

      <div
        className={styles.history}
        ref={historyRef}
        data-testid={testIds.chatPanel.history}
        role="log"
        aria-live="polite"
        aria-relevant="additions text"
        aria-label="Conversation with the Gardenio Assistant"
      >
        {turns.length === 0 ? (
          <div className={styles.empty}>
            <p>Ask about your gardenio dashboards, metrics, and logs.</p>
            <div className={styles.suggestions}>
              {SUGGESTIONS.map((s) => (
                <Button
                  key={s}
                  variant="secondary"
                  size="sm"
                  disabled={!llmReady || isGenerating}
                  onClick={() => sendMessage(s)}
                >
                  {s}
                </Button>
              ))}
            </div>
          </div>
        ) : (
          turns.map((turn, idx) => (
            <Message
              key={idx}
              turn={turn}
              isLast={idx === turns.length - 1}
              isGenerating={isGenerating}
              styles={styles}
            />
          ))
        )}
      </div>

      {sendError && (
        <Alert title="Assistant error" severity="error" data-testid={testIds.chatPage.error}>
          {sendError}
        </Alert>
      )}

      <div className={styles.composer}>
        <TextArea
          data-testid={testIds.chatPanel.input}
          aria-label="Message the Gardenio Assistant"
          value={input}
          onChange={(e) => setInput(e.currentTarget.value)}
          onKeyDown={onKeyDown}
          placeholder="Ask the gardenio assistant… (Enter to send, Shift+Enter for newline)"
          disabled={!llmReady || isGenerating}
          rows={2}
        />
        {isGenerating ? (
          <Button
            data-testid={testIds.chatPanel.stop}
            aria-label="Stop generating"
            variant="secondary"
            icon="square-shape"
            onClick={stopGenerating}
          >
            Stop
          </Button>
        ) : (
          <Button
            data-testid={testIds.chatPanel.send}
            aria-label="Send message"
            onClick={() => sendMessage(input)}
            disabled={!input.trim() || !llmReady}
            icon="message"
          >
            Send
          </Button>
        )}
      </div>
    </div>
  );
}

interface MessageProps {
  turn: ChatTurn;
  isLast: boolean;
  isGenerating: boolean;
  styles: ReturnType<typeof getStyles>;
}

function Message({ turn, isLast, isGenerating, styles }: MessageProps) {
  const isUser = turn.role === 'user';
  const showTyping = !isUser && isLast && isGenerating && !turn.content;
  return (
    <div className={isUser ? styles.userRow : styles.assistantRow} data-testid={testIds.chatPanel.message}>
      <div className={isUser ? styles.userBubble : styles.assistantBubble}>
        {turn.toolCalls && turn.toolCalls.length > 0 && (
          <div className={styles.toolCalls}>
            {turn.toolCalls.map((tc, i) => (
              <span
                key={`${tc.name}-${i}`}
                className={styles.toolPill}
                data-testid={testIds.chatPanel.toolCall}
                title={tc.error ? tc.error : tc.arguments}
                aria-label={
                  tc.running
                    ? `Running tool ${tc.name}`
                    : tc.error
                      ? `Tool ${tc.name} failed: ${tc.error}`
                      : `Tool ${tc.name} completed`
                }
              >
                {tc.running ? <Spinner size="sm" inline /> : <Icon name={tc.error ? 'exclamation-triangle' : 'check'} />}
                &nbsp;{tc.name}
              </span>
            ))}
          </div>
        )}
        {showTyping ? (
          <span className={styles.typing} role="status" aria-label="Assistant is typing">
            …
          </span>
        ) : isUser ? (
          // User input is rendered verbatim (pre-wrap), never as markdown/HTML.
          <span className={styles.content}>{turn.content}</span>
        ) : (
          <AssistantMarkdown content={turn.content} className={styles.content} />
        )}
      </div>
    </div>
  );
}

/**
 * Render assistant output as sanitized markdown so the tables, fenced code
 * blocks, and dashboard/Explore deeplinks the system prompt promises actually
 * render (item 3). `renderMarkdown` from @grafana/data routes through Grafana's
 * `sanitizeTextPanelContent` (xss.FilterXSS with an allowlist + CSS filter) by
 * default — script tags, event-handler attrs, and `javascript:` URLs are
 * stripped — so the model's text (which may itself be shaped by untrusted tool
 * output) cannot inject active content. This is the ONLY HTML-bearing render
 * path in the chat surface; user input never reaches it.
 */
function AssistantMarkdown({ content, className }: { content: string; className: string }) {
  const html = useMemo(() => renderMarkdown(content), [content]);
  return <div className={className} dangerouslySetInnerHTML={{ __html: html }} />;
}

const getStyles = (theme: GrafanaTheme2) => ({
  container: css`
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(1.5)};
    height: calc(100vh - ${theme.spacing(24)});
    min-height: 480px;
  `,
  header: css`
    display: flex;
    align-items: center;
    justify-content: space-between;
  `,
  brand: css`
    display: flex;
    align-items: center;
    gap: ${theme.spacing(1)};
  `,
  title: css`
    font-size: ${theme.typography.h4.fontSize};
    font-weight: ${theme.typography.fontWeightMedium};
  `,
  toolBadge: css`
    display: flex;
    align-items: center;
  `,
  history: css`
    flex: 1;
    overflow-y: auto;
    padding: ${theme.spacing(2)};
    border: 1px solid ${theme.colors.border.weak};
    border-radius: ${theme.shape.radius.default};
    background: ${theme.colors.background.secondary};
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(1.5)};
  `,
  empty: css`
    margin: auto;
    text-align: center;
    color: ${theme.colors.text.secondary};
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(2)};
  `,
  suggestions: css`
    display: flex;
    flex-wrap: wrap;
    gap: ${theme.spacing(1)};
    justify-content: center;
  `,
  userRow: css`
    display: flex;
    justify-content: flex-end;
  `,
  assistantRow: css`
    display: flex;
    justify-content: flex-start;
  `,
  userBubble: css`
    max-width: 85%;
    padding: ${theme.spacing(1, 1.5)};
    border-radius: ${theme.shape.radius.default};
    background: ${theme.colors.primary.main};
    color: ${theme.colors.primary.contrastText};
    white-space: pre-wrap;
    word-break: break-word;
  `,
  assistantBubble: css`
    max-width: 90%;
    padding: ${theme.spacing(1, 1.5)};
    border-radius: ${theme.shape.radius.default};
    background: ${theme.colors.background.primary};
    border: 1px solid ${theme.colors.border.weak};
    white-space: pre-wrap;
    word-break: break-word;

    /* Tighten default margins on the markdown block so a single paragraph
       answer doesn't gain extra vertical padding inside the bubble. */
    & p:first-child {
      margin-top: 0;
    }
    & p:last-child {
      margin-bottom: 0;
    }
    /* Tables and code blocks the system prompt promises: keep them readable
       inside the constrained bubble width. */
    & table {
      border-collapse: collapse;
      margin: ${theme.spacing(1, 0)};
    }
    & th,
    & td {
      border: 1px solid ${theme.colors.border.weak};
      padding: ${theme.spacing(0.5, 1)};
      text-align: left;
    }
    & pre {
      overflow-x: auto;
      padding: ${theme.spacing(1)};
      border-radius: ${theme.shape.radius.default};
      background: ${theme.colors.background.secondary};
    }
    & code {
      font-family: ${theme.typography.fontFamilyMonospace};
      font-size: ${theme.typography.bodySmall.fontSize};
    }
    & a {
      color: ${theme.colors.text.link};
    }
  `,
  content: css`
    font-size: ${theme.typography.body.fontSize};
  `,
  typing: css`
    opacity: 0.6;
    font-size: ${theme.typography.h3.fontSize};
    letter-spacing: 2px;
  `,
  toolCalls: css`
    display: flex;
    flex-wrap: wrap;
    gap: ${theme.spacing(0.5)};
    margin-bottom: ${theme.spacing(1)};
  `,
  toolPill: css`
    display: inline-flex;
    align-items: center;
    font-size: ${theme.typography.bodySmall.fontSize};
    font-family: ${theme.typography.fontFamilyMonospace};
    padding: ${theme.spacing(0.25, 0.75)};
    border-radius: ${theme.shape.radius.pill};
    background: ${theme.colors.background.secondary};
    border: 1px solid ${theme.colors.border.weak};
    color: ${theme.colors.text.secondary};
  `,
  composer: css`
    display: flex;
    gap: ${theme.spacing(1)};
    align-items: flex-end;
  `,
});
