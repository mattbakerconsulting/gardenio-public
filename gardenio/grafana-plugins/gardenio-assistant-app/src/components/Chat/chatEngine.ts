import { llm, mcp } from '@grafana/llm';
import type { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { CallToolResultSchema, type Tool as MCPTool } from '@modelcontextprotocol/sdk/types.js';
import {
  fromEvent,
  lastValueFrom,
  NEVER,
  type Observable,
  partition,
  share,
  startWith,
  type Subscription,
  takeUntil,
  timeout,
} from 'rxjs';

import { buildSystemPrompt } from './systemPrompt';
import { isToolAllowed } from '../../tools/readOnlyTools';

/** A single tool invocation, surfaced to the UI so the user can see what ran. */
export interface RenderedToolCall {
  name: string;
  arguments: string;
  running: boolean;
  error?: string;
  responseText?: string;
}

export interface RunChatOptions {
  /** Connected MCP client (from useMCPClient). */
  client: Client;
  /** Curated tool surface offered to the model (read-only, +writes when enabled). */
  tools: MCPTool[];
  /** Full prior conversation (user/assistant turns) excluding the system prompt. */
  history: Array<{ role: 'user' | 'assistant'; content: string }>;
  /** The new user message. */
  userMessage: string;
  /**
   * Whether dashboard/folder writes are enabled for this env (plugin config
   * `enableDashboardWrites`, default false). Drives BOTH the system prompt
   * (read-only vs write-enabled capabilities) and the call-time egress guard
   * (`isToolAllowed` admits the vetted write tools only when true). C8,
   * ka-0000005589.
   */
  enableWrites?: boolean;
  /** Model abstraction; defaults to LARGE for complex grounded reasoning. */
  model?: llm.Model;
  /** Called whenever the in-progress assistant reply text grows. */
  onAssistantContent: (content: string) => void;
  /** Called whenever the set of tool calls changes. */
  onToolCalls?: (calls: RenderedToolCall[]) => void;
  /** Aborts the turn (e.g. on component unmount); tears down live subscriptions. */
  signal?: AbortSignal;
  /**
   * Per-stream stall timeout in ms. A LiteLLM/Bedrock stream that fails to send
   * a first chunk — or stalls mid-stream without a done/finish sentinel — rejects
   * the turn after this long instead of hanging `isGenerating` forever. Overridable
   * for tests; defaults to {@link DEFAULT_STREAM_TIMEOUT_MS}.
   */
  streamTimeoutMs?: number;
}

/**
 * Hard cap on tool-calling rounds. The model normally converges in a handful of
 * rounds; this guards against a misbehaving model (or a tool that always errors
 * and is retried forever) holding the turn open indefinitely.
 */
export const MAX_TOOL_ROUNDS = 10;

/**
 * Default per-stream stall timeout (ms). Generous enough for a cold Bedrock
 * first token and slow tool-grounded rounds, while still bounding a wedged
 * stream so the input never locks permanently. Applied to BOTH the first chunk
 * (`first`) and the idle gap between chunks (`each`), so a stream that goes
 * silent after partial content also rejects.
 */
export const DEFAULT_STREAM_TIMEOUT_MS = 90_000;

const isFunctionToolCall = (tc: llm.ToolCall): boolean => tc.type === 'function';

/**
 * Build the message array for a turn: system prompt, prior history, new user
 * message. The system prompt mode follows `enableWrites` (read-only by default,
 * write-enabled when the env opts in). Exported for testing.
 */
export function buildMessages(
  history: Array<{ role: 'user' | 'assistant'; content: string }>,
  userMessage: string,
  enableWrites = false
): llm.Message[] {
  return [
    { role: 'system', content: buildSystemPrompt(enableWrites) },
    ...history.map((m) => ({ role: m.role, content: m.content })),
    { role: 'user', content: userMessage },
  ];
}

/**
 * Run one chat turn with the streaming tool-calling loop, mirroring the
 * official grafana-llm-app reference implementation (DevSandboxChat):
 *
 *  1. Stream a completion with the curated tools attached.
 *  2. Partition the stream into tool-call chunks vs. content chunks; accumulate
 *     content into the live assistant reply.
 *  3. If the model emitted tool calls, execute them against the MCP client,
 *     push the results back as `tool` messages, and stream again — repeating
 *     until the model stops requesting tools or MAX_TOOL_ROUNDS is reached.
 *
 * Resilience (ka-0000000846): each stream is wired, before partition, with
 *  - `timeout`: a stalled stream rejects instead of hanging the turn (item 2);
 *  - `takeUntil(abort$)`: unmount / SPA-nav completes the stream so the Grafana
 *    Live channel is torn down rather than running to completion in the
 *    background (item 1). On abort the tool branch's `lastValueFrom` resolves
 *    (the stream completes, `accumulateToolCalls` emits an empty set), so the
 *    loop exits cleanly without a dangling Live channel;
 *  - `share()`: `partition()` subscribes its source twice (tool-call + content
 *    branch). Grafana Live already multicasts; `share()` makes that contract
 *    explicit so both branches observe the same emissions (item 9).
 *
 * Tool-surface enforcement is defense-in-depth and respects the write gate: the
 * caller hands only the curated surface to the model (advertise-time — read-only
 * tools, plus the vetted write tools when `enableWrites`), AND `executeToolCall`
 * re-checks `isToolAllowed(name, enableWrites)` at the egress before any
 * `client.callTool` (call-time). So an off-list name the model might emit — or a
 * vetted write tool when writes are OFF — can never be invoked from here.
 */
export async function runChatTurn(opts: RunChatOptions): Promise<void> {
  const { client, tools, history, userMessage, onAssistantContent, onToolCalls, signal } = opts;
  const enableWrites = opts.enableWrites ?? false;
  const model = opts.model ?? llm.Model.LARGE;
  const streamTimeoutMs = opts.streamTimeoutMs ?? DEFAULT_STREAM_TIMEOUT_MS;
  const openAITools = mcp.convertToolsToOpenAI(tools);
  const toolCalls = new Map<string, RenderedToolCall>();

  // Notifier that completes the in-flight stream on abort. When `signal` is
  // absent we use NEVER so `takeUntil` is a no-op rather than completing early.
  const abort$ = signal ? fromEvent(signal, 'abort') : NEVER;

  const emitToolCalls = () => onToolCalls?.(Array.from(toolCalls.values()));

  const messages = buildMessages(history, userMessage, enableWrites);

  // Text committed from prior streaming rounds. Each round's content is shown
  // as `committed + currentRoundText`, so narration emitted before a tool call
  // is preserved across rounds rather than overwritten.
  let committed = '';
  let currentRoundText = '';

  // A SINGLE live content subscription, replaced each round (item 8): completed
  // per-round subscriptions are no longer held in an array until end-of-turn.
  // `teardown` unsubscribes it; on abort that — together with `takeUntil(abort$)`
  // completing the source — closes the Grafana Live channel immediately (item 1).
  let currentContentSub: Subscription | undefined;

  const startStream = (
    withStartMarker: boolean
  ): Observable<llm.ChatCompletionsResponse<llm.ChatCompletionsChunk>> => {
    currentRoundText = '';
    const stream = llm
      .streamChatCompletions({ model, messages, tools: openAITools })
      .pipe(timeout({ first: streamTimeoutMs, each: streamTimeoutMs }), takeUntil(abort$), share());

    const [toolCallsStream, otherMessages] = partition(stream, (chunk) =>
      llm.isToolCallsMessage(chunk.choices[0].delta)
    );

    // On follow-up rounds, seed an empty assistant content message so
    // accumulateContent has a starting point even if the model leads with
    // another tool call. Crucially we re-prefix with `committed`, so the seed
    // re-renders prior rounds' text instead of blanking the bubble.
    const firstMessage: Partial<llm.ChatCompletionsResponse<llm.ChatCompletionsChunk>> = {
      choices: [{ delta: { role: 'assistant', content: '' } }],
    };

    const contentMessages = withStartMarker
      ? otherMessages.pipe(
          // @ts-expect-error startWith seed is a partial chunk, matching the reference impl.
          startWith(firstMessage),
          llm.accumulateContent()
        )
      : otherMessages.pipe(llm.accumulateContent());

    currentContentSub?.unsubscribe();
    currentContentSub = contentMessages.subscribe({
      next: (content) => {
        currentRoundText = content;
        onAssistantContent(committed + content);
      },
      // The content branch shares the stream with the tool branch, so it also
      // observes stream errors (e.g. TimeoutError). Swallow them here — the tool
      // branch's `lastValueFrom` rejection is the single path that surfaces the
      // error to the caller; without this handler rxjs logs a spurious uncaught
      // error on every failed stream (item 6).
      error: () => {},
    });
    return toolCallsStream;
  };

  const teardown = () => {
    currentContentSub?.unsubscribe();
    currentContentSub = undefined;
  };
  signal?.addEventListener('abort', teardown, { once: true });

  try {
    // If the caller passed an already-aborted signal, `fromEvent(signal,'abort')`
    // never fires (the DOM event has already passed) so `takeUntil(abort$)` would
    // be a no-op and we'd wait the full per-stream timeout before the tool branch
    // resolves. Short-circuit before kicking off any stream.
    if (signal?.aborted) {
      return;
    }
    let toolCallsStream = startStream(false);
    let toolCallMessages = await lastValueFrom(toolCallsStream.pipe(llm.accumulateToolCalls()));

    let round = 0;
    while (toolCallMessages.tool_calls.length > 0) {
      if (signal?.aborted) {
        return;
      }
      if (round >= MAX_TOOL_ROUNDS) {
        onAssistantContent(
          `${committed}${currentRoundText}\n\n_(Stopped after ${MAX_TOOL_ROUNDS} tool rounds.)_`
        );
        return;
      }

      // Normalize + dedupe the tool calls so the UI map key, the assistant
      // tool-call message, and each tool result all agree even when the model
      // emits empty or duplicate ids (item 7). An empty id would otherwise
      // collapse distinct calls into one UI pill and produce a tool_call_id
      // that no tool result references.
      const functionCalls: llm.ToolCall[] = [];
      const seenIds = new Set<string>();
      toolCallMessages.tool_calls.filter(isFunctionToolCall).forEach((fc, i) => {
        // Synthetic id uses double-underscore sentinels so it cannot collide with
        // a model-emitted id that happens to look like `tool-call-{n}-{m}`.
        const id = fc.id && fc.id.length > 0 ? fc.id : `__synthetic_tool_call_${round}_${i}__`;
        if (seenIds.has(id)) {
          return;
        }
        seenIds.add(id);
        functionCalls.push({ ...fc, id });
      });

      // Push the assistant tool-call message with the normalized/deduped calls
      // (not the raw set) so every tool_call has exactly one matching result.
      messages.push({ ...toolCallMessages, tool_calls: functionCalls });

      round++;

      await Promise.all(
        functionCalls.map((fc) =>
          executeToolCall(fc, client, toolCalls, emitToolCalls, messages, enableWrites)
        )
      );

      // Fold this round's narration into the committed prefix so it survives
      // the next round's fresh content accumulation.
      if (currentRoundText) {
        committed = `${committed}${currentRoundText}\n\n`;
      }

      toolCallsStream = startStream(true);
      toolCallMessages = await lastValueFrom(toolCallsStream.pipe(llm.accumulateToolCalls()));
    }
  } finally {
    teardown();
    signal?.removeEventListener('abort', teardown);
  }
}

/**
 * Extract the dashboard UID an `update_dashboard` call targets. mcp-grafana
 * accepts the UID either as the top-level `uid` arg OR lifted from the dashboard
 * body (`dashboard.uid`) when the top-level arg is absent — mirroring upstream's
 * `uid := args.UID; if uid == "" { uid = spec["uid"] }`. We check both so the
 * create-only guard cannot be bypassed by smuggling the uid into the body.
 * Returns the trimmed uid, or '' when no uid is targeted (a pure CREATE — the
 * server mints the uid, so there is nothing to overwrite).
 */
export function extractDashboardUid(args: Record<string, unknown>): string {
  const top = typeof args.uid === 'string' ? args.uid.trim() : '';
  if (top) {
    return top;
  }
  const dashboard = args.dashboard;
  if (dashboard && typeof dashboard === 'object') {
    const nested = (dashboard as Record<string, unknown>).uid;
    if (typeof nested === 'string') {
      return nested.trim();
    }
  }
  return '';
}

/**
 * Extract the legacy numeric `id` an `update_dashboard` body addresses, if any.
 * Grafana's classic save resolves an EXISTING dashboard by this internal id: a
 * null/absent/zero id means CREATE, a positive id overwrites that dashboard in
 * place — and mcp-grafana forwards the body's id unchanged. So a non-zero id is
 * an overwrite-target the uid oracle cannot see; we surface it here so the guard
 * can hard-reject it.
 *
 * Returns the id (as a display string) when the body addresses an existing
 * dashboard, or null for a create (id null/absent/0, or not a usable number).
 * Accepts a numeric string id too (e.g. "42"), since arg JSON shapes vary.
 */
export function extractDashboardBodyId(args: Record<string, unknown>): string | null {
  const dashboard = args.dashboard;
  if (!dashboard || typeof dashboard !== 'object') {
    return null;
  }
  const raw = (dashboard as Record<string, unknown>).id;
  if (typeof raw === 'number') {
    return Number.isFinite(raw) && raw !== 0 ? String(raw) : null;
  }
  if (typeof raw === 'string') {
    const n = Number(raw.trim());
    return Number.isFinite(n) && n !== 0 ? raw.trim() : null;
  }
  return null;
}

/**
 * Hard, arg-level CREATE-ONLY guard for `update_dashboard` (C8 hardening,
 * ka-0000002896). `update_dashboard` is the ONLY dashboard-write tool upstream
 * mcp-grafana exposes, and it is create-OR-overwrite-by-UID — so the name-level
 * write gate (`isToolAllowed`) admitting it is NOT sufficient: a model error, or
 * a prompt-injection routed through a tool result, could reuse an EXISTING uid
 * and silently clobber a live dashboard the managed-SA can write. This control
 * bounds the tool to CREATE — a call whose uid already resolves to a dashboard
 * is rejected (recorded as a tool error) BEFORE the write reaches
 * `client.callTool`.
 *
 * Grafana resolves an EXISTING dashboard by THREE channels, and a create-only
 * guard must close all three. Real upstream behavior (grafana-llm-app 1.0.8
 * vendors mcp-grafana v0.11.3; Grafana v11.3):
 *
 *  - mcp-grafana's full-JSON save (updateDashboardWithFullJSON) builds a
 *    SaveDashboardCommand{Dashboard, FolderUID, Overwrite} and passes the body
 *    plus the model-controlled top-level `overwrite` straight to PostDashboard —
 *    no uid/id/title stripping on that path. (v0.11.3 has NO v2 spec[uid]
 *    lifting, NO uid stripping, and NO origID restore — earlier comments here
 *    described a newer mcp-grafana that this version does not ship.)
 *  - Grafana's save then resolves an existing row by (1) numeric id, (2) uid, or
 *    (3) title+folder when overwrite=true (getExistingDashboardByTitleAndFolder:
 *    `org_id=? AND title=?` + folder constraint; on a match with overwrite=true it
 *    SetID/SetUID/SetVersion of the existing dashboard -> overwrite-in-place).
 *
 * Three identity channels, three checks:
 *
 *  1. Body `id` channel (checked FIRST, no read needed). Grafana's save resolves
 *     an existing dashboard by the embedded numeric `dashboard.id` (`id: null`
 *     CREATES; a non-null id overwrites THAT dashboard in place), and mcp-grafana
 *     forwards the body's id unchanged. So an id-bearing body with NO uid would
 *     otherwise sail past the uid oracle below and clobber that id. Because an
 *     id-bearing body is, by Grafana's contract, never a fresh create, we
 *     HARD-REJECT it outright — there is no by-id read tool on the allow-list to
 *     use as an existence oracle, and none is needed.
 *
 *  2. `overwrite` channel (checked SECOND, no read needed). The title+folder
 *     resolution above fires ONLY under overwrite=true, and the LLM fully controls
 *     that flag. So `{dashboard:{title:"Production Overview",...},overwrite:true}`
 *     with NO uid and NO id would otherwise pass channels 1 and 3 as a "pure
 *     CREATE" and silently overwrite the existing "Production Overview"
 *     (search_dashboards is read-only-allow-listed, so an injected LLM can
 *     enumerate titles first). A create-only tool never needs overwrite=true, so
 *     we HARD-REJECT it — closing the title+folder channel with no read, and
 *     defense-in-depth on the uid/id channels. With overwrite false/absent a title
 *     collision merely 412s (ErrDashboardWithSameNameInFolderExists) and the model
 *     retries a fresh title.
 *
 *  3. uid channel (existence pre-check). Direction of the gate (fail toward NOT
 *     overwriting):
 *      - No uid targeted            -> pure CREATE; permitted (nothing to clobber).
 *      - uid read SUCCEEDS (exists) -> REJECTED; this would overwrite. Covers
 *                                      patch-mode (`operations`), which always
 *                                      mutates an existing dashboard.
 *      - uid read fails / not-found -> uid is not a live dashboard; permitted, so
 *                                      a CREATE that names its own fresh uid still
 *                                      works (acceptance criterion 1). A confirmed
 *                                      EXISTING dashboard is the only state hard-
 *                                      blocked here. We use the read tool the
 *                                      plugin already trusts, `get_dashboard_by_uid`,
 *                                      as the existence oracle.
 *
 * Returns an error string when the call must be rejected, or null to proceed.
 */
export async function dashboardCreateOnlyViolation(
  name: string,
  args: Record<string, unknown>,
  client: Client
): Promise<string | null> {
  if (name !== 'update_dashboard') {
    return null;
  }

  // Channel 1: an id-bearing body always targets an existing dashboard for
  // overwrite (Grafana create requires id null/absent). Hard-reject before the
  // uid oracle so a `{dashboard:{id:42,...}}` body with no uid cannot bypass it.
  const bodyId = extractDashboardBodyId(args);
  if (bodyId !== null) {
    return (
      `update_dashboard is bounded to CREATE-only: the dashboard body carries a numeric id ` +
      `(${bodyId}), which targets an existing dashboard for overwrite. Create a new dashboard ` +
      `(omit the id, or set id: null) instead.`
    );
  }

  // Channel 2: top-level `overwrite` flag (hard reject, no read needed). Grafana
  // resolves an existing dashboard by a THIRD identity channel that the uid/id
  // checks cannot see — same title in the same folder — but ONLY when the save
  // carries overwrite=true (getExistingDashboardByTitleAndFolder then SetID/
  // SetUID/SetVersion of the existing row; with overwrite false/absent a title
  // collision instead 412s as ErrDashboardWithSameNameInFolderExists). mcp-grafana
  // forwards the model-controlled top-level `overwrite` straight into
  // SaveDashboardCommand.Overwrite. A CREATE-only tool never needs overwrite=true:
  // with it false/absent a title collision simply fails and the model retries a
  // fresh title. So we reject overwrite=true outright — closing the title+folder
  // channel with no read, and adding defense-in-depth on the uid/id channels.
  // Checked at the TOP LEVEL of args (where UpdateDashboardParams.Overwrite lives),
  // not inside dashboard{}. A string "true" is treated the same (arg JSON shapes
  // vary); rejecting more is always safe, since a genuine create never sets it.
  const overwrite = args.overwrite;
  if (overwrite === true || (typeof overwrite === 'string' && overwrite.trim().toLowerCase() === 'true')) {
    return (
      `update_dashboard is bounded to CREATE-only: overwrite=true is not permitted, because Grafana ` +
      `resolves an existing dashboard by title+folder under overwrite=true and would overwrite it in ` +
      `place. Create a new dashboard with a title that does not already exist (omit overwrite, or set ` +
      `overwrite: false) instead.`
    );
  }

  // Channel 3: uid existence pre-check.
  const uid = extractDashboardUid(args);
  if (!uid) {
    return null;
  }

  // Existence pre-check. A SUCCESSFUL read means the uid is a live dashboard and
  // the write would overwrite it. Any failure (not-found, transient) leaves the
  // uid unconfirmed-existing, so we permit — the create flow must still succeed.
  let exists = false;
  try {
    const response = await client.callTool({ name: 'get_dashboard_by_uid', arguments: { uid } });
    const result = CallToolResultSchema.parse(response);
    exists = result.isError !== true;
  } catch {
    exists = false;
  }

  if (exists) {
    return (
      `update_dashboard is bounded to CREATE-only: a dashboard with uid "${uid}" already exists, ` +
      `so overwriting it is not permitted. Create a new dashboard (omit the uid, or choose a fresh ` +
      `uid that does not already exist) instead.`
    );
  }
  return null;
}

/**
 * Execute a single tool call against the MCP client, record its state for the
 * UI, and push the result back into the message array as a `tool` message.
 *
 * Egress guard: even though the model is only ever advertised the curated
 * surface, we re-assert `isToolAllowed(name, enableWrites)` here so an off-list
 * name the model might emit (hallucination, or injected via a tool result) — or
 * a vetted write tool when writes are OFF — can never reach `client.callTool`.
 * This is the plugin-side half of the tool envelope.
 *
 * Arg-level guard: for the one create-OR-overwrite write tool we admit
 * (`update_dashboard`), `dashboardCreateOnlyViolation` additionally rejects any
 * call that targets an EXISTING dashboard via any of Grafana's three resolution
 * channels — a numeric body id, overwrite=true (the title+folder channel), or a
 * uid that already resolves — bounding the tool to CREATE so it cannot clobber a
 * live dashboard (C8, ka-0000002896).
 */
async function executeToolCall(
  fc: llm.ToolCall,
  client: Client,
  toolCalls: Map<string, RenderedToolCall>,
  emitToolCalls: () => void,
  messages: llm.Message[],
  enableWrites = false
): Promise<void> {
  const { function: f, id } = fc;
  toolCalls.set(id, { name: f.name, arguments: f.arguments, running: true });
  emitToolCalls();

  const recordError = (error: string) => {
    messages.push({ role: 'tool', tool_call_id: id, content: error });
    toolCalls.set(id, { name: f.name, arguments: f.arguments, running: false, error });
    emitToolCalls();
  };

  // Egress allow-list re-check (defense-in-depth — see doc comment). With writes
  // off this admits only the read-only set; with writes on it additionally
  // admits the vetted dashboard/folder write tools, never anything else.
  if (!isToolAllowed(f.name, enableWrites)) {
    recordError(`Tool "${f.name}" is not on the allowed tool list and was not executed.`);
    return;
  }

  let args: Record<string, unknown> = {};
  try {
    args = f.arguments ? JSON.parse(f.arguments) : {};
  } catch {
    // The model occasionally emits invalid JSON; surface it as a tool error
    // rather than throwing and aborting the whole turn.
    recordError(`Invalid tool arguments (not valid JSON): ${f.arguments}`);
    return;
  }

  // Arg-level CREATE-only guard (C8, ka-0000002896). For update_dashboard, reject
  // a call that targets a live dashboard via any of Grafana's three resolution
  // channels (numeric body id, overwrite=true title+folder, or an existing uid) —
  // bounding the one create-OR-overwrite write tool to CREATE so it cannot clobber
  // an existing dashboard. Recorded via recordError like any other tool failure, so
  // the rejection surfaces in the UI and the model can self-correct to a fresh
  // identity. Only reached when the name-level write gate already admitted the tool.
  const createOnlyViolation = await dashboardCreateOnlyViolation(f.name, args, client);
  if (createOnlyViolation) {
    recordError(createOnlyViolation);
    return;
  }

  try {
    const response = await client.callTool({ name: f.name, arguments: args });
    const toolResult = CallToolResultSchema.parse(response);
    const textContent = toolResult.content
      .filter((c): c is { type: 'text'; text: string } => c.type === 'text')
      .map((c) => c.text)
      .join('');
    messages.push({ role: 'tool', tool_call_id: id, content: textContent });
    toolCalls.set(id, {
      name: f.name,
      arguments: f.arguments,
      running: false,
      responseText: textContent,
    });
    emitToolCalls();
  } catch (e) {
    recordError(e instanceof Error ? e.message : String(e));
  }
}
