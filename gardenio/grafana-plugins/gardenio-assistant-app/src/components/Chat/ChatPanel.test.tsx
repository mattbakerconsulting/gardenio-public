import React from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Subject } from 'rxjs';

const listTools = jest.fn();
const llmEnabled = jest.fn();
const streamChatCompletions = jest.fn();
// A STABLE client reference: useMCPClient returns a context-cached client in
// production, so the mock must too — otherwise the panel's useAsync re-runs
// every render and never settles.
const stableClient = { listTools };

// Mock @grafana/llm at the transport boundary only: real accumulators / real
// isToolCallsMessage / real convertToolsToOpenAI (item 15 spirit also applies
// here — bind to upstream semantics not a hand-rolled replica). `mcpEnabled`
// is controlled via a top-level `let` so individual tests can flip the
// "grafana-llm-app absent" path (item 10).
let mcpEnabled = true;
jest.mock('@grafana/llm', () => {
  const actual = jest.requireActual('@grafana/llm');
  return {
    ...actual,
    llm: {
      ...actual.llm,
      enabled: () => llmEnabled(),
      streamChatCompletions: (...args: unknown[]) => streamChatCompletions(...args),
    },
    mcp: {
      ...actual.mcp,
      useMCPClient: () => ({ client: stableClient, enabled: mcpEnabled }),
    },
  };
});

import { ChatPanel } from './ChatPanel';
import { testIds } from '../testIds';

// Grafana's testId convention keeps the whole "data-testid <id>" string as the
// rendered attribute VALUE, so getByTestId resolves against the constant itself.

describe('ChatPanel', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mcpEnabled = true;
    listTools.mockResolvedValue({
      tools: [
        { name: 'query_prometheus', description: 'q', inputSchema: { type: 'object' } },
        { name: 'update_dashboard', description: 'w', inputSchema: { type: 'object' } },
        { name: 'search_dashboards', description: 's', inputSchema: { type: 'object' } },
      ],
    });
  });

  it('renders gardenio branding and the read-only badge (writes off by default)', async () => {
    llmEnabled.mockResolvedValue(true);
    render(<ChatPanel />);
    expect(await screen.findByText('Gardenio Assistant')).toBeInTheDocument();
    expect(screen.getByText('read-only')).toBeInTheDocument();
    // The write badge is NOT shown in the default read-only posture.
    expect(screen.queryByText('read + dashboard write')).not.toBeInTheDocument();
  });

  it('curates the tool surface down to read-only tools when writes are off', async () => {
    llmEnabled.mockResolvedValue(true);
    render(<ChatPanel />);
    // 3 advertised tools (incl. update_dashboard) -> only the 2 read-only ones
    // survive curation while writes are off.
    await waitFor(() => expect(screen.getByText('2 tools')).toBeInTheDocument());
    expect(listTools).toHaveBeenCalled();
  });

  // --- Item 4 (panel-level): with enableDashboardWrites the vetted write tool
  // joins the advertised surface and the badge flips to read+write. ---
  it('with writes ON, exposes read tools + the vetted write tool and shows the write badge', async () => {
    llmEnabled.mockResolvedValue(true);
    render(<ChatPanel enableDashboardWrites />);
    expect(await screen.findByText('Gardenio Assistant')).toBeInTheDocument();
    // Badge flips to read + dashboard write; the read-only badge is gone.
    expect(screen.getByText('read + dashboard write')).toBeInTheDocument();
    expect(screen.queryByText('read-only')).not.toBeInTheDocument();
    // 3 advertised (query_prometheus, update_dashboard, search_dashboards) -> all
    // 3 survive: 2 read + the vetted update_dashboard write tool.
    await waitFor(() => expect(screen.getByText('3 tools')).toBeInTheDocument());
  });

  it('with writes ON, still drops UNVETTED write tools from the surface', async () => {
    llmEnabled.mockResolvedValue(true);
    listTools.mockResolvedValue({
      tools: [
        { name: 'query_prometheus', description: 'q', inputSchema: { type: 'object' } },
        { name: 'update_dashboard', description: 'w', inputSchema: { type: 'object' } },
        { name: 'create_folder', description: 'f', inputSchema: { type: 'object' } },
        { name: 'create_incident', description: 'c', inputSchema: { type: 'object' } },
        { name: 'delete_dashboard', description: 'd', inputSchema: { type: 'object' } },
      ],
    });
    render(<ChatPanel enableDashboardWrites />);
    // query_prometheus + update_dashboard + create_folder survive (3); the
    // unvetted create_incident / delete_dashboard are netted by the heuristic.
    await waitFor(() => expect(screen.getByText('3 tools')).toBeInTheDocument());
  });

  it('warns when the LLM provider is not configured', async () => {
    llmEnabled.mockResolvedValue(false);
    render(<ChatPanel />);
    expect(await screen.findByText('LLM provider not configured')).toBeInTheDocument();
  });

  it('disables the input until the LLM is ready', async () => {
    llmEnabled.mockResolvedValue(false);
    render(<ChatPanel />);
    await screen.findByText('LLM provider not configured');
    expect(screen.getByTestId(testIds.chatPanel.input)).toBeDisabled();
  });

  // --- Item 10: degrade gracefully when grafana-llm-app is fully absent. The
  // panel must not crash; it should surface the "LLM provider not configured"
  // warning and keep the composer disabled. ---
  it('degrades gracefully when grafana-llm-app is absent (mcp disabled, llm disabled)', async () => {
    mcpEnabled = false;
    llmEnabled.mockResolvedValue(false);
    render(<ChatPanel />);
    expect(await screen.findByText('LLM provider not configured')).toBeInTheDocument();
    expect(screen.getByTestId(testIds.chatPanel.input)).toBeDisabled();
    // 0 tools is the natural state — listTools is never called when mcp is off.
    await waitFor(() => expect(screen.getByText('0 tools')).toBeInTheDocument());
    expect(listTools).not.toHaveBeenCalled();
  });

  // --- Item 11: when LLM is ready but no curated tools survived, surface the
  // reduced-grounding state so users know answers aren't grounded. ---
  it('surfaces a "Grounding tools unavailable" alert when llmReady && tools.length===0', async () => {
    llmEnabled.mockResolvedValue(true);
    listTools.mockResolvedValue({
      // Only write-shaped tools -> curation filters everything out.
      tools: [
        { name: 'update_dashboard', description: 'w', inputSchema: { type: 'object' } },
        { name: 'create_incident', description: 'c', inputSchema: { type: 'object' } },
      ],
    });
    render(<ChatPanel />);
    await waitFor(() => expect(screen.getByText('0 tools')).toBeInTheDocument());
    expect(screen.getByTestId(testIds.chatPanel.noTools)).toBeInTheDocument();
    expect(screen.getByText(/Grounding tools unavailable/i)).toBeInTheDocument();
  });

  // --- Item 5: a11y. Streaming chat history must be a live region screen
  // readers can pick up, and interactive controls must be labelled. ---
  it('history is an a11y live region (role=log, aria-live=polite)', async () => {
    llmEnabled.mockResolvedValue(true);
    render(<ChatPanel />);
    const history = await screen.findByTestId(testIds.chatPanel.history);
    expect(history.getAttribute('role')).toBe('log');
    expect(history.getAttribute('aria-live')).toBe('polite');
    expect(history.getAttribute('aria-relevant')).toBe('additions text');
    expect(history.getAttribute('aria-label')).toMatch(/Gardenio Assistant/i);
  });

  it('TextArea and Send button carry aria-labels', async () => {
    llmEnabled.mockResolvedValue(true);
    render(<ChatPanel />);
    await screen.findByText('Gardenio Assistant');
    expect(screen.getByLabelText(/Message the Gardenio Assistant/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/Send message/i)).toBeInTheDocument();
  });

  // --- Item 4: a Stop control replaces Send during generation, calls abort,
  // and re-enables the composer. ---
  it('shows a Stop button while generating; Stop aborts and re-enables Send', async () => {
    llmEnabled.mockResolvedValue(true);
    // A never-completing stream: the turn stays in-flight until aborted.
    const live$ = new Subject<unknown>();
    streamChatCompletions.mockReturnValue(live$.asObservable());

    render(<ChatPanel />);
    await waitFor(() => expect(screen.getByText('2 tools')).toBeInTheDocument());

    const input = screen.getByTestId(testIds.chatPanel.input) as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: 'hi' } });
    fireEvent.click(screen.getByTestId(testIds.chatPanel.send));

    // Stop button appears (Send disappears).
    const stop = await screen.findByTestId(testIds.chatPanel.stop);
    expect(stop).toBeInTheDocument();
    expect(stop.getAttribute('aria-label')).toMatch(/Stop generating/i);
    expect(screen.queryByTestId(testIds.chatPanel.send)).not.toBeInTheDocument();

    // Click Stop -> composer returns to Send.
    await act(async () => {
      fireEvent.click(stop);
    });
    await waitFor(() => expect(screen.getByTestId(testIds.chatPanel.send)).toBeInTheDocument());
    expect(screen.queryByTestId(testIds.chatPanel.stop)).not.toBeInTheDocument();
  });

  // --- Item 3: assistant content is rendered through @grafana/data
  // renderMarkdown (DOMPurify-sanitized) so tables / fenced code / links the
  // system prompt promises actually render, while preserving the sanitize
  // envelope (no script execution, no event handlers reaching the DOM). ---
  it('renders assistant markdown (links + code) and SANITIZES script tags', async () => {
    llmEnabled.mockResolvedValue(true);
    // One content chunk carrying mixed markdown: a link, inline code, and a
    // <script> + onerror payload that the sanitizer must strip.
    const payload =
      'See the [dashboard](/d/abc) and `query_prometheus` — <script>window.__pwned=1</script><img src=x onerror="window.__pwned2=1">';
    const stream$ = new Subject<unknown>();
    streamChatCompletions.mockReturnValue(stream$.asObservable());

    render(<ChatPanel />);
    await waitFor(() => expect(screen.getByText('2 tools')).toBeInTheDocument());

    const input = screen.getByTestId(testIds.chatPanel.input) as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: 'q' } });
    fireEvent.click(screen.getByTestId(testIds.chatPanel.send));

    // Emit the content chunk, then complete the stream so the turn finishes.
    await act(async () => {
      stream$.next({ choices: [{ delta: { role: 'assistant', content: payload } }] });
      stream$.complete();
    });

    // Link renders as an anchor (markdown active).
    const link = await screen.findByRole('link', { name: /dashboard/i });
    expect(link.getAttribute('href')).toBe('/d/abc');
    // Inline code renders as <code>.
    expect(screen.getByText('query_prometheus').tagName.toLowerCase()).toBe('code');
    // Script tag is stripped (no live script element in the DOM, no global set).
    expect(document.querySelector('script')).toBeNull();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
    // onerror on the <img> is stripped (DOMPurify removes event-handler attrs).
    const imgs = document.querySelectorAll('img');
    for (const img of imgs) {
      expect(img.getAttribute('onerror')).toBeNull();
    }
  });
});
