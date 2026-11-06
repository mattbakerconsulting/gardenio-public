import { Subject, of } from 'rxjs';

// Item 15: exercise the REAL @grafana/llm streaming accumulators
// (accumulateContent / accumulateToolCalls / isToolCallsMessage) and mock ONLY
// the transport boundary, streamChatCompletions. The previous version
// reimplemented the llm streaming semantics by hand, which would silently drift
// if upstream changed the chunk format; binding to the real operators makes the
// test break loudly on such a change instead.
const streamChatCompletions = jest.fn();

jest.mock('@grafana/llm', () => {
  const actual = jest.requireActual('@grafana/llm');
  return {
    ...actual,
    llm: {
      ...actual.llm,
      // Only the transport is mocked; the accumulators below come from `actual`.
      streamChatCompletions: (...args: unknown[]) => streamChatCompletions(...args),
    },
    mcp: {
      ...actual.mcp,
      // convertToolsToOpenAI is real — see the allow-list advertise-time test.
    },
  };
});

import { llm } from '@grafana/llm';
import {
  buildMessages,
  dashboardCreateOnlyViolation,
  extractDashboardBodyId,
  extractDashboardUid,
  runChatTurn,
  MAX_TOOL_ROUNDS,
} from './chatEngine';
import { SYSTEM_PROMPT, buildSystemPrompt } from './systemPrompt';

// Chunk shapes mirror grafana-llm-app's streaming wire format. The real
// accumulators in @grafana/llm consume exactly these.
type Chunk = llm.ChatCompletionsResponse<llm.ChatCompletionsChunk>;
const contentChunk = (content: string): Chunk =>
  ({ choices: [{ delta: { role: 'assistant', content } }] }) as unknown as Chunk;
const toolChunk = (name: string, id: string, args = '{}'): Chunk =>
  ({
    choices: [{ delta: { tool_calls: [{ id, index: 0, type: 'function', function: { name, arguments: args } }] } }],
  }) as unknown as Chunk;

describe('chatEngine.buildMessages', () => {
  it('prepends the gardenio system prompt (read-only by default)', () => {
    const msgs = buildMessages([], 'hello');
    expect(msgs[0]).toEqual({ role: 'system', content: SYSTEM_PROMPT });
  });

  it('uses the write-enabled system prompt when enableWrites is true', () => {
    const msgs = buildMessages([], 'hello', true);
    expect(msgs[0]).toEqual({ role: 'system', content: buildSystemPrompt(true) });
    // The write-enabled system prompt differs from the read-only default.
    expect(msgs[0].content).not.toBe(SYSTEM_PROMPT);
  });

  it('threads prior history between the system prompt and the new user message', () => {
    const history = [
      { role: 'user' as const, content: 'first question' },
      { role: 'assistant' as const, content: 'first answer' },
    ];
    const msgs = buildMessages(history, 'second question');
    expect(msgs.map((m) => m.role)).toEqual(['system', 'user', 'assistant', 'user']);
    expect(msgs[1].content).toBe('first question');
    expect(msgs[2].content).toBe('first answer');
    expect(msgs[msgs.length - 1]).toEqual({ role: 'user', content: 'second question' });
  });
});

describe('systemPrompt', () => {
  it('grounds the assistant in gardenio observability and declares read-only by default', () => {
    expect(SYSTEM_PROMPT).toMatch(/gardenio/i);
    expect(SYSTEM_PROMPT).toMatch(/read-only/i);
    expect(SYSTEM_PROMPT).toMatch(/dashboard/i);
    // The default prompt must NOT advertise create power.
    expect(SYSTEM_PROMPT).not.toMatch(/create and update grafana dashboards/i);
  });

  it('advertises dashboard/folder create-update ONLY when writes are enabled', () => {
    const prompt = buildSystemPrompt(true);
    expect(prompt).toMatch(/create and update grafana dashboards/i);
    expect(prompt).toMatch(/create_folder/);
    expect(prompt).toMatch(/update_dashboard/);
    // Bounded surface: it must say it cannot touch alerts/datasources/etc.
    expect(prompt).toMatch(/cannot create or modify alerts/i);
    // The in-cluster / no-public-internet line is preserved in write mode.
    expect(prompt).toMatch(/inside the gardenio cluster/i);
    expect(prompt).toMatch(/no access to the\n?\s*public internet/i);
  });

  it('keeps all traffic in-cluster in BOTH modes', () => {
    expect(SYSTEM_PROMPT).toMatch(/inside the gardenio cluster/i);
    expect(buildSystemPrompt(true)).toMatch(/inside the gardenio cluster/i);
  });
});

describe('chatEngine.runChatTurn', () => {
  const makeClient = (callTool = jest.fn()) => ({ callTool }) as any;

  beforeEach(() => {
    streamChatCompletions.mockReset();
  });

  it('streams a plain answer with no tools', async () => {
    streamChatCompletions.mockReturnValueOnce(of(contentChunk('Hello'), contentChunk(' world')));
    const onAssistantContent = jest.fn();
    await runChatTurn({
      client: makeClient(),
      tools: [],
      history: [],
      userMessage: 'hi',
      onAssistantContent,
    });
    expect(onAssistantContent).toHaveBeenLastCalledWith('Hello world');
  });

  it('runs a read-only tool then streams the grounded answer', async () => {
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('query_prometheus', 'tc1', '{"expr":"up"}')))
      .mockReturnValueOnce(of(contentChunk('CPU is fine')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: '42' }] });
    const onAssistantContent = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      history: [],
      userMessage: 'cpu?',
      onAssistantContent,
    });
    expect(callTool).toHaveBeenCalledWith({ name: 'query_prometheus', arguments: { expr: 'up' } });
    expect(onAssistantContent).toHaveBeenLastCalledWith('CPU is fine');
  });

  it('BLOCKS a write tool at the egress when writes are OFF (default)', async () => {
    // Round 1: model (mis)emits a write tool. Round 2: model answers.
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('update_dashboard', 'tc1', '{"uid":"x"}')))
      .mockReturnValueOnce(of(contentChunk('done')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'ok' }] });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      history: [],
      userMessage: 'make a dashboard',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // The write tool must NEVER reach client.callTool when writes are off.
    expect(callTool).not.toHaveBeenCalled();
    // It is surfaced as a rejected tool call.
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toMatch(/not on the allowed tool list/);
  });

  // --- Item 4c: call-time guard admits the vetted write tools ONLY when writes
  // are enabled. With enableWrites=true the model's update_dashboard call must
  // actually reach client.callTool. ---
  it('ADMITS a vetted write tool at the egress when writes are ON', async () => {
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('update_dashboard', 'tc1', '{"dashboard":{"title":"New"}}')))
      .mockReturnValueOnce(of(contentChunk('created')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'uid-123' }] });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'make a dashboard',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // The vetted write tool reaches client.callTool with its parsed args.
    expect(callTool).toHaveBeenCalledWith({
      name: 'update_dashboard',
      arguments: { dashboard: { title: 'New' } },
    });
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toBeUndefined();
  });

  it('still BLOCKS an UNVETTED write tool at the egress even when writes are ON', async () => {
    // create_incident is write-shaped but NOT on WRITE_TOOLS — the gate must not
    // widen to it just because dashboard writes are enabled.
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('create_incident', 'tc1', '{"title":"x"}')))
      .mockReturnValueOnce(of(contentChunk('done')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'ok' }] });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'declare an incident',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    expect(callTool).not.toHaveBeenCalled();
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'create_incident', running: false });
    expect(lastCalls[0].error).toMatch(/not on the allowed tool list/);
  });

  // --- C8 hardening (ka-0000002896): update_dashboard is bounded to CREATE-only.
  // The name-level write gate admits it when writes are ON; an additional
  // arg-level guard must REJECT a call whose uid resolves to an EXISTING
  // dashboard (overwrite) and PERMIT a create (new/absent uid). ---
  it('CREATE-only guard: REJECTS update_dashboard when the uid already exists (writes ON)', async () => {
    streamChatCompletions
      .mockReturnValueOnce(
        of(toolChunk('update_dashboard', 'tc1', '{"uid":"existing","dashboard":{"title":"Clobber"}}'))
      )
      .mockReturnValueOnce(of(contentChunk('done')));
    // The existence pre-check (get_dashboard_by_uid) SUCCEEDS -> uid exists.
    const callTool = jest.fn().mockImplementation(({ name }: { name: string }) => {
      if (name === 'get_dashboard_by_uid') {
        return Promise.resolve({ content: [{ type: 'text', text: '{"dashboard":{"uid":"existing"}}' }] });
      }
      return Promise.resolve({ content: [{ type: 'text', text: 'SHOULD NOT BE REACHED' }] });
    });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'overwrite the prod dashboard',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // The existence pre-check ran with the targeted uid...
    expect(callTool).toHaveBeenCalledWith({ name: 'get_dashboard_by_uid', arguments: { uid: 'existing' } });
    // ...but the overwrite write NEVER reached client.callTool.
    const updateCalls = callTool.mock.calls.filter(([a]: [{ name: string }]) => a.name === 'update_dashboard');
    expect(updateCalls).toHaveLength(0);
    // It is surfaced as a rejected tool call with the create-only reason.
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toMatch(/CREATE-only/);
  });

  it('CREATE-only guard: PERMITS update_dashboard when the uid does not exist (writes ON)', async () => {
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('update_dashboard', 'tc1', '{"uid":"fresh","dashboard":{"title":"New"}}')))
      .mockReturnValueOnce(of(contentChunk('created')));
    // The existence pre-check returns a tool error (not-found) -> uid is free.
    const callTool = jest.fn().mockImplementation(({ name }: { name: string }) => {
      if (name === 'get_dashboard_by_uid') {
        return Promise.resolve({ content: [{ type: 'text', text: 'dashboard not found' }], isError: true });
      }
      return Promise.resolve({ content: [{ type: 'text', text: 'uid-fresh' }] });
    });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'make a dashboard with uid fresh',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // The create proceeded to client.callTool with its parsed args.
    expect(callTool).toHaveBeenCalledWith({
      name: 'update_dashboard',
      arguments: { uid: 'fresh', dashboard: { title: 'New' } },
    });
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toBeUndefined();
  });

  it('CREATE-only guard: PERMITS update_dashboard with NO uid (pure create) — no pre-check (writes ON)', async () => {
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('update_dashboard', 'tc1', '{"dashboard":{"title":"New"}}')))
      .mockReturnValueOnce(of(contentChunk('created')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'uid-123' }] });
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'make a dashboard',
      onAssistantContent: jest.fn(),
    });
    // A pure create needs no existence pre-check — the server mints the uid.
    expect(callTool).not.toHaveBeenCalledWith(expect.objectContaining({ name: 'get_dashboard_by_uid' }));
    expect(callTool).toHaveBeenCalledWith({
      name: 'update_dashboard',
      arguments: { dashboard: { title: 'New' } },
    });
  });

  it('CREATE-only guard: REJECTS an id-bearing body with NO uid (numeric-id overwrite channel, writes ON)', async () => {
    // Adversarial bypass: Grafana resolves an existing dashboard by the embedded
    // numeric id, so {dashboard:{id:42}} with no uid would clobber dashboard 42.
    // The guard must hard-reject it WITHOUT a uid oracle read.
    streamChatCompletions
      .mockReturnValueOnce(
        of(toolChunk('update_dashboard', 'tc1', '{"dashboard":{"id":42,"title":"Clobber"},"overwrite":true}'))
      )
      .mockReturnValueOnce(of(contentChunk('done')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'SHOULD NOT BE REACHED' }] });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'overwrite dashboard 42',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // No oracle read (id channel needs none) and the write never reached callTool.
    expect(callTool).not.toHaveBeenCalled();
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toMatch(/CREATE-only/);
    expect(lastCalls[0].error).toMatch(/numeric id/);
  });

  it('CREATE-only guard: REJECTS overwrite:true with NO uid/NO id (title+folder overwrite channel, writes ON)', async () => {
    // Adversarial bypass (gardener QC): Grafana resolves an existing dashboard by
    // title+folder under overwrite=true, so {dashboard:{title:"Production
    // Overview"},overwrite:true} with no uid/no id would clobber the live
    // dashboard. The guard must hard-reject it WITHOUT a uid oracle read.
    streamChatCompletions
      .mockReturnValueOnce(
        of(
          toolChunk(
            'update_dashboard',
            'tc1',
            '{"dashboard":{"title":"Production Overview","panels":[]},"overwrite":true}'
          )
        )
      )
      .mockReturnValueOnce(of(contentChunk('done')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'SHOULD NOT BE REACHED' }] });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'overwrite the Production Overview dashboard by title',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // No oracle read (overwrite channel needs none) and the write never reached callTool.
    expect(callTool).not.toHaveBeenCalled();
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toMatch(/CREATE-only/);
    expect(lastCalls[0].error).toMatch(/overwrite=true/);
  });

  it('CREATE-only guard: PERMITS update_dashboard when the oracle THROWS (transient failure -> create still succeeds, writes ON)', async () => {
    // Acceptance criterion: CREATE must still succeed. A transient oracle failure
    // must NOT block the write (fail toward create).
    streamChatCompletions
      .mockReturnValueOnce(of(toolChunk('update_dashboard', 'tc1', '{"uid":"maybe","dashboard":{"title":"New"}}')))
      .mockReturnValueOnce(of(contentChunk('created')));
    const callTool = jest.fn().mockImplementation(({ name }: { name: string }) => {
      if (name === 'get_dashboard_by_uid') {
        return Promise.reject(new Error('transient datasource error'));
      }
      return Promise.resolve({ content: [{ type: 'text', text: 'uid-maybe' }] });
    });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      enableWrites: true,
      history: [],
      userMessage: 'make a dashboard with uid maybe',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // Despite the oracle throwing, the create proceeded to client.callTool.
    expect(callTool).toHaveBeenCalledWith({
      name: 'update_dashboard',
      arguments: { uid: 'maybe', dashboard: { title: 'New' } },
    });
    const lastCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    expect(lastCalls[0]).toMatchObject({ name: 'update_dashboard', running: false });
    expect(lastCalls[0].error).toBeUndefined();
  });

  it('preserves narration across tool rounds (no blanking)', async () => {
    streamChatCompletions
      .mockReturnValueOnce(of(contentChunk('Let me check. '), toolChunk('query_prometheus', 'tc1')))
      .mockReturnValueOnce(of(contentChunk('The answer is 5.')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: '5' }] });
    const onAssistantContent = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      history: [],
      userMessage: 'q',
      onAssistantContent,
    });
    // Final rendered content must retain round-1 narration AND round-2 answer.
    expect(onAssistantContent).toHaveBeenLastCalledWith('Let me check. \n\nThe answer is 5.');
  });

  it('exposes a finite tool-round cap', () => {
    expect(MAX_TOOL_ROUNDS).toBeGreaterThan(0);
    expect(MAX_TOOL_ROUNDS).toBeLessThanOrEqual(20);
  });

  it('does not throw when started with an already-aborted signal', async () => {
    const onAssistantContent = jest.fn();
    const controller = new AbortController();
    controller.abort();
    // The key invariant is that an already-aborted signal tears down without
    // throwing. (The stronger guarantee — that no stream is even opened — is
    // covered by the "returns immediately" test below.)
    await expect(
      runChatTurn({
        client: makeClient(),
        tools: [],
        history: [],
        userMessage: 'q',
        signal: controller.signal,
        onAssistantContent,
      })
    ).resolves.toBeUndefined();
  });

  // --- Item 14: advertise-time allow-list. The curated tools handed to the
  // model must be EXACTLY the names the caller passed — runChatTurn must never
  // smuggle an extra (e.g. write) tool into the streamed request. Call-time
  // enforcement is covered by the "BLOCKS a write tool" test above; this covers
  // the advertise-time half. ---
  it('advertises ONLY the curated tools to streamChatCompletions', async () => {
    streamChatCompletions.mockReturnValueOnce(of(contentChunk('ok')));
    const curated = [
      { name: 'query_prometheus', description: 'q', inputSchema: { type: 'object', properties: {} } },
      { name: 'search_dashboards', description: 's', inputSchema: { type: 'object', properties: {} } },
    ] as any;
    await runChatTurn({
      client: makeClient(),
      tools: curated,
      history: [],
      userMessage: 'hi',
      onAssistantContent: jest.fn(),
    });
    const request = streamChatCompletions.mock.calls[0][0];
    const advertised = (request.tools ?? []).map((t: any) => t.function?.name ?? t.name);
    expect(advertised).toEqual(['query_prometheus', 'search_dashboards']);
    // And every advertised entry is a function tool (OpenAI shape).
    for (const t of request.tools ?? []) {
      expect(t.type).toBe('function');
    }
  });

  // --- Item 1: abort cancels the in-flight stream. With a never-completing
  // Subject as the stream, the turn would hang forever; aborting the signal
  // must complete it so runChatTurn resolves rather than wedging. ---
  it('abort mid-stream tears down the live stream and resolves the turn', async () => {
    const live$ = new Subject<Chunk>();
    streamChatCompletions.mockReturnValueOnce(live$.asObservable());
    const controller = new AbortController();
    const onAssistantContent = jest.fn();

    const turn = runChatTurn({
      client: makeClient(),
      tools: [],
      history: [],
      userMessage: 'q',
      signal: controller.signal,
      onAssistantContent,
    });

    // Emit some partial content, then abort before any done sentinel arrives.
    live$.next(contentChunk('partial'));
    controller.abort();

    // The turn must resolve (not hang) once aborted, even though the underlying
    // stream never completed on its own.
    await expect(turn).resolves.toBeUndefined();
    expect(onAssistantContent).toHaveBeenCalledWith('partial');
  });

  // --- Item 2: a stalled stream (no done/finish sentinel) must reject via the
  // per-stream timeout instead of hanging the turn forever. ---
  describe('per-stream timeout', () => {
    // Keep the fake-timer toggle in a hook so a thrown assertion never leaves
    // subsequent tests running under fake timers (which causes mysterious hangs).
    beforeEach(() => jest.useFakeTimers());
    afterEach(() => jest.useRealTimers());

    it('rejects a stalled stream via the per-stream timeout', async () => {
      const live$ = new Subject<Chunk>();
      streamChatCompletions.mockReturnValueOnce(live$.asObservable());

      const turn = runChatTurn({
        client: makeClient(),
        tools: [],
        history: [],
        userMessage: 'q',
        streamTimeoutMs: 1000,
        onAssistantContent: jest.fn(),
      });
      // Attach the rejection expectation BEFORE advancing timers so the
      // rejection is observed (no unhandled rejection).
      const assertion = expect(turn).rejects.toThrow(/[Tt]imeout/);
      // Never emit anything; let the first-chunk timeout fire.
      jest.advanceTimersByTime(1500);
      await assertion;
    });
  });

  // --- Pre-aborted signal short-circuits before any stream is opened
  // (adversarial-review finding: avoid the full per-stream timeout wait when
  // the caller passes an already-aborted signal). ---
  it('returns immediately when the signal is already aborted', async () => {
    const controller = new AbortController();
    controller.abort();
    await runChatTurn({
      client: makeClient(),
      tools: [],
      history: [],
      userMessage: 'q',
      signal: controller.signal,
      onAssistantContent: jest.fn(),
    });
    // No stream is ever requested when pre-aborted.
    expect(streamChatCompletions).not.toHaveBeenCalled();
  });

  // --- Item 7: tool calls with empty/duplicate ids are normalized so each
  // surfaced call is distinct and every tool result has a matching id. ---
  it('normalizes empty/duplicate tool-call ids into distinct calls', async () => {
    // Round 1: two function calls share an empty id. They must not collapse into
    // one UI pill, and both must execute.
    streamChatCompletions
      .mockReturnValueOnce(
        of(toolChunk('query_prometheus', '', '{"expr":"up"}'), {
          choices: [
            { delta: { tool_calls: [{ id: '', index: 1, type: 'function', function: { name: 'search_dashboards', arguments: '{}' } }] } },
          ],
        } as unknown as Chunk)
      )
      .mockReturnValueOnce(of(contentChunk('done')));
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'ok' }] });
    const onToolCalls = jest.fn();
    await runChatTurn({
      client: makeClient(callTool),
      tools: [],
      history: [],
      userMessage: 'q',
      onAssistantContent: jest.fn(),
      onToolCalls,
    });
    // Both distinct tools executed despite sharing an empty id.
    expect(callTool).toHaveBeenCalledTimes(2);
    const finalCalls = onToolCalls.mock.calls.at(-1)?.[0] ?? [];
    const names = finalCalls.map((c: { name: string }) => c.name).sort();
    expect(names).toEqual(['query_prometheus', 'search_dashboards']);
  });
});

// --- C8 hardening (ka-0000002896): focused unit tests for the create-only
// guard helpers, exercising the uid-extraction and existence-oracle logic
// directly (the runChatTurn tests above cover the integration path). ---
describe('extractDashboardUid', () => {
  it('reads the top-level uid arg', () => {
    expect(extractDashboardUid({ uid: 'abc' })).toBe('abc');
  });

  it('lifts the uid from the dashboard body when no top-level uid (upstream parity)', () => {
    expect(extractDashboardUid({ dashboard: { uid: 'nested', title: 'x' } })).toBe('nested');
  });

  it('prefers the top-level uid over a nested one', () => {
    expect(extractDashboardUid({ uid: 'top', dashboard: { uid: 'nested' } })).toBe('top');
  });

  it('trims surrounding whitespace', () => {
    expect(extractDashboardUid({ uid: '  spaced  ' })).toBe('spaced');
  });

  it('returns "" for a pure create (no uid anywhere)', () => {
    expect(extractDashboardUid({ dashboard: { title: 'New' } })).toBe('');
    expect(extractDashboardUid({})).toBe('');
  });

  it('ignores a non-string / empty uid', () => {
    expect(extractDashboardUid({ uid: 123 as unknown as string })).toBe('');
    expect(extractDashboardUid({ uid: '   ' })).toBe('');
    expect(extractDashboardUid({ dashboard: { uid: 42 as unknown as string } })).toBe('');
  });
});

describe('extractDashboardBodyId', () => {
  it('returns the numeric id when the body addresses an existing dashboard', () => {
    expect(extractDashboardBodyId({ dashboard: { id: 42, title: 'x' } })).toBe('42');
  });

  it('accepts a numeric string id', () => {
    expect(extractDashboardBodyId({ dashboard: { id: '7' } })).toBe('7');
    expect(extractDashboardBodyId({ dashboard: { id: ' 7 ' } })).toBe('7');
  });

  it('treats id null / absent / 0 as a create (returns null)', () => {
    expect(extractDashboardBodyId({ dashboard: { id: null, title: 'New' } })).toBeNull();
    expect(extractDashboardBodyId({ dashboard: { title: 'New' } })).toBeNull();
    expect(extractDashboardBodyId({ dashboard: { id: 0 } })).toBeNull();
    expect(extractDashboardBodyId({ dashboard: { id: '0' } })).toBeNull();
    expect(extractDashboardBodyId({})).toBeNull();
  });

  it('ignores a non-numeric / unparsable id', () => {
    expect(extractDashboardBodyId({ dashboard: { id: 'abc' } })).toBeNull();
    expect(extractDashboardBodyId({ dashboard: { id: {} as unknown as number } })).toBeNull();
  });
});

describe('dashboardCreateOnlyViolation', () => {
  const makeClient = (callTool = jest.fn()) => ({ callTool }) as any;

  it('returns null for any tool that is not update_dashboard (no pre-check)', async () => {
    const callTool = jest.fn();
    expect(await dashboardCreateOnlyViolation('create_folder', { uid: 'x' }, makeClient(callTool))).toBeNull();
    expect(await dashboardCreateOnlyViolation('query_prometheus', {}, makeClient(callTool))).toBeNull();
    expect(callTool).not.toHaveBeenCalled();
  });

  it('returns null for a pure create (no uid) without calling the existence oracle', async () => {
    const callTool = jest.fn();
    expect(await dashboardCreateOnlyViolation('update_dashboard', { dashboard: { title: 'New' } }, makeClient(callTool))).toBeNull();
    expect(callTool).not.toHaveBeenCalled();
  });

  it('REJECTS when the existence oracle confirms the uid exists', async () => {
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: '{"dashboard":{}}' }] });
    const violation = await dashboardCreateOnlyViolation('update_dashboard', { uid: 'live' }, makeClient(callTool));
    expect(callTool).toHaveBeenCalledWith({ name: 'get_dashboard_by_uid', arguments: { uid: 'live' } });
    expect(violation).toMatch(/CREATE-only/);
    expect(violation).toMatch(/live/);
  });

  it('REJECTS a nested-uid overwrite (cannot be bypassed by smuggling uid into the body)', async () => {
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: '{"dashboard":{}}' }] });
    const violation = await dashboardCreateOnlyViolation(
      'update_dashboard',
      { dashboard: { uid: 'sneaky', title: 'x' } },
      makeClient(callTool)
    );
    expect(callTool).toHaveBeenCalledWith({ name: 'get_dashboard_by_uid', arguments: { uid: 'sneaky' } });
    expect(violation).toMatch(/CREATE-only/);
  });

  it('REJECTS a patch-mode overwrite (operations always target an existing dashboard)', async () => {
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: '{"dashboard":{}}' }] });
    const violation = await dashboardCreateOnlyViolation(
      'update_dashboard',
      { uid: 'live', operations: [{ op: 'replace', path: '/title', value: 'x' }] },
      makeClient(callTool)
    );
    expect(violation).toMatch(/CREATE-only/);
  });

  it('REJECTS an id-bearing body with NO uid WITHOUT consulting the oracle (numeric-id overwrite channel)', async () => {
    const callTool = jest.fn();
    const violation = await dashboardCreateOnlyViolation(
      'update_dashboard',
      { dashboard: { id: 42, title: 'Clobber' }, overwrite: true },
      makeClient(callTool)
    );
    expect(violation).toMatch(/CREATE-only/);
    expect(violation).toMatch(/numeric id/);
    expect(violation).toMatch(/42/);
    // The id channel is a hard reject — no uid oracle read is performed.
    expect(callTool).not.toHaveBeenCalled();
  });

  it('REJECTS an id-bearing body even when a fresh uid would otherwise pass the oracle', async () => {
    // id wins over the uid oracle: a free uid does NOT rescue an id-bearing body.
    const callTool = jest.fn().mockResolvedValue({ content: [{ type: 'text', text: 'not found' }], isError: true });
    const violation = await dashboardCreateOnlyViolation(
      'update_dashboard',
      { uid: 'fresh', dashboard: { id: 99, uid: 'fresh', title: 'x' } },
      makeClient(callTool)
    );
    expect(violation).toMatch(/CREATE-only/);
    expect(callTool).not.toHaveBeenCalled();
  });

  it('PERMITS a body with id: null (explicit create shape)', async () => {
    const callTool = jest.fn();
    expect(
      await dashboardCreateOnlyViolation('update_dashboard', { dashboard: { id: null, title: 'New' } }, makeClient(callTool))
    ).toBeNull();
    expect(callTool).not.toHaveBeenCalled();
  });

  it('REJECTS overwrite:true with NO uid and NO id (title+folder overwrite channel) WITHOUT consulting the oracle', async () => {
    // Adversarial bypass: Grafana resolves an existing dashboard by title+folder
    // when overwrite=true, so {dashboard:{title:"Production Overview"},overwrite:true}
    // with no uid/no id would clobber the live "Production Overview". The guard must
    // hard-reject it WITHOUT a uid oracle read (no uid to read anyway).
    const callTool = jest.fn();
    const violation = await dashboardCreateOnlyViolation(
      'update_dashboard',
      { dashboard: { title: 'Production Overview', panels: [] }, overwrite: true },
      makeClient(callTool)
    );
    expect(violation).toMatch(/CREATE-only/);
    expect(violation).toMatch(/overwrite=true/);
    // The overwrite channel is a hard reject — no uid oracle read is performed.
    expect(callTool).not.toHaveBeenCalled();
  });

  it('REJECTS overwrite as the string "true" (arg JSON shapes vary)', async () => {
    const callTool = jest.fn();
    const violation = await dashboardCreateOnlyViolation(
      'update_dashboard',
      { dashboard: { title: 'X' }, overwrite: 'true' },
      makeClient(callTool)
    );
    expect(violation).toMatch(/CREATE-only/);
    expect(violation).toMatch(/overwrite=true/);
    expect(callTool).not.toHaveBeenCalled();
  });

  it('PERMITS overwrite:false (a genuine create never overwrites)', async () => {
    const callTool = jest.fn();
    expect(
      await dashboardCreateOnlyViolation(
        'update_dashboard',
        { dashboard: { title: 'New' }, overwrite: false },
        makeClient(callTool)
      )
    ).toBeNull();
    expect(callTool).not.toHaveBeenCalled();
  });

  it('REJECTS when the existence oracle returns a successful-but-empty result (predicate keys on isError, not content)', async () => {
    // Pins the security-load-bearing predicate `exists = result.isError !== true`:
    // an empty-but-successful read still counts as EXISTS, so the guard must NOT
    // fail open if the result content is empty.
    const callTool = jest.fn().mockResolvedValue({ content: [] });
    const violation = await dashboardCreateOnlyViolation('update_dashboard', { uid: 'live' }, makeClient(callTool));
    expect(callTool).toHaveBeenCalledWith({ name: 'get_dashboard_by_uid', arguments: { uid: 'live' } });
    expect(violation).toMatch(/CREATE-only/);
  });

  it('PERMITS when the existence oracle reports the uid as not-found (isError result)', async () => {
    const callTool = jest
      .fn()
      .mockResolvedValue({ content: [{ type: 'text', text: 'not found' }], isError: true });
    expect(await dashboardCreateOnlyViolation('update_dashboard', { uid: 'fresh' }, makeClient(callTool))).toBeNull();
  });

  it('PERMITS (fails open toward create) when the existence oracle throws', async () => {
    const callTool = jest.fn().mockRejectedValue(new Error('transient'));
    expect(await dashboardCreateOnlyViolation('update_dashboard', { uid: 'maybe' }, makeClient(callTool))).toBeNull();
  });
});
