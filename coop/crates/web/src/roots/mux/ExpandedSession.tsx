import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import { Terminal as XTerm } from "@xterm/xterm";
import { type RefObject, useEffect, useRef, useState } from "react";
import { InspectorSidebar, type WsEventListener } from "@/components/inspector/InspectorSidebar";
import { StreamAlert } from "@/components/StreamAlert";
import { Terminal } from "@/components/Terminal";
import { TerminalLayout } from "@/components/TerminalLayout";
import { formatLabels, sessionSubtitle, sessionTitle } from "@/components/Tile";
import { apiGet } from "@/hooks/useApiClient";
import { type ConnectionStatus, type SocketLike, WsRpc } from "@/hooks/useWebSocket";
import { useInit } from "@/hooks/utils";
import { renderAnsiPre } from "@/lib/ansi-render";
import { b64decode } from "@/lib/base64";
import { EXPANDED_FONT_SIZE, MONO_FONT, THEME } from "@/lib/constants";
import { FallbackSocket } from "@/lib/fallback-socket";
import { ReplayGate } from "@/lib/replay-gate";
import type { PromptContext, SessionInfo, WsMessage } from "@/lib/types";
import { WsMessageHarness } from "@/lib/ws-harness";

interface ExpandedSessionProps {
  info: SessionInfo;
  sidebarWidth: number;
  muxSend: (msg: unknown) => void;
  sendInputRef: RefObject<((text: string) => void) | null>;
  onTransition: (
    sessionId: string,
    next: string,
    prompt: PromptContext | null,
    lastMessage: string | null,
  ) => void;
  onClose: () => void;
  version?: string;
}

export function ExpandedSession({
  info,
  sidebarWidth,
  muxSend,
  sendInputRef,
  onTransition,
  onClose,
  version,
}: ExpandedSessionProps) {
  const wsRef = useRef<SocketLike | null>(null);
  const rpcRef = useRef<WsRpc | null>(null);
  const gateRef = useRef(new ReplayGate());
  const harnessRef = useRef(new WsMessageHarness());
  const lastDiagnoseCountRef = useRef(0);
  const diagnoseTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Reconnect bookkeeping. The per-session WS drops on ForwardAuth cookie
  // expiry, upstream pod recycle, or transient blips; without auto-reconnect
  // the expanded terminal strands on "disconnected" until a manual reload.
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const cancelledRef = useRef(false);
  const [wsStatus, setWsStatus] = useState<ConnectionStatus>("disconnected");
  const [ready, setReady] = useState(false);
  const [prompt, setPrompt] = useState<PromptContext | null>(null);
  const [lastMessage, setLastMessage] = useState<string | null>(null);
  const [showStreamAlert, setShowStreamAlert] = useState<string | null>(null);
  const wsListenersRef = useRef(new Set<WsEventListener>());
  const onTransitionRef = useRef(onTransition);
  onTransitionRef.current = onTransition;

  // Stable XTerm + FitAddon, created once on mount
  const [{ term, fit }] = useState(() => {
    const t = new XTerm({
      scrollback: 10000,
      fontSize: EXPANDED_FONT_SIZE,
      fontFamily: MONO_FONT,
      theme: THEME,
      cursorBlink: false,
      cursorInactiveStyle: "none",
      disableStdin: true,
      convertEol: false,
    });
    const f = new FitAddon();
    t.loadAddon(f);
    return { term: t, fit: f };
  });

  const cleanupRef = useRef<(() => void) | null>(null);

  // Initialize handlers (runs once during first render)
  useInit(() => {
    (window as unknown as Record<string, unknown>).__wsHarness = harnessRef.current;
    info.term = term;
    info.fit = fit;

    const dataDisp = term.onData((data) => {
      // Normal keystrokes are operator-tier `input:send` on BOTH transports.
      // The direct per-session WS previously sent `input:send:raw`, which the
      // edge now reserves for admin (raw byte injection == REST /input/raw),
      // so an edge-authed operator got keystrokes refused. `input:send {text}`
      // writes byte-identical PTY input while keeping the operator tier.
      if (wsRef.current?.readyState === FallbackSocket.OPEN) {
        wsRef.current.send(JSON.stringify(inputSendFrame(info.id, data)));
      } else {
        muxSend(inputSendFrame(info.id, data));
      }
    });

    const resizeDisp = term.onResize(({ cols, rows }) => {
      if (wsRef.current?.readyState === FallbackSocket.OPEN) {
        wsRef.current.send(JSON.stringify({ event: "resize", cols, rows }));
      }
    });

    sendInputRef.current = (text: string) => {
      // Operator-tier `input:send` on the direct WS too (see term.onData above).
      if (wsRef.current?.readyState === FallbackSocket.OPEN) {
        wsRef.current.send(JSON.stringify(inputSendFrame(info.id, text)));
      } else {
        muxSend(inputSendFrame(info.id, text));
      }
    };

    cleanupRef.current = () => {
      (window as unknown as Record<string, unknown>).__wsHarness = undefined;
      cancelledRef.current = true;
      if (reconnectTimerRef.current !== null) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
      dataDisp.dispose();
      resizeDisp.dispose();
      rpcRef.current?.dispose();
      rpcRef.current = null;
      wsRef.current?.close();
      wsRef.current = null;
      if (info.webgl) {
        info.webgl.dispose();
        info.webgl = null;
      }
      term.dispose();
      info.term = null;
      info.fit = null;
      sendInputRef.current = null;
    };
  });

  // Cleanup on unmount
  useEffect(() => () => cleanupRef.current?.(), []);

  function scheduleDiagnose() {
    if (diagnoseTimerRef.current !== null) return;
    diagnoseTimerRef.current = setTimeout(() => {
      diagnoseTimerRef.current = null;
      const issues = harnessRef.current.diagnose();
      if (issues.length > lastDiagnoseCountRef.current) {
        lastDiagnoseCountRef.current = issues.length;
        setShowStreamAlert(issues[0].message);
        setTimeout(() => setShowStreamAlert(null), 10_000);
      }
    }, 2_000);
  }

  function connectWs() {
    gateRef.current.reset();
    harnessRef.current.reconnect();
    lastDiagnoseCountRef.current = 0;
    setShowStreamAlert(null);
    setWsStatus("connecting");
    // FallbackSocket tries a real WS first, then SSE+POST if the upgrade is
    // dropped (AVA). It derives /sse/session/{id} from this path and carries
    // the ?subscribe flags + ?token through to both transports.
    const ws = new FallbackSocket(`/ws/${info.id}?subscribe=pty,state,usage,hooks`);
    wsRef.current = ws;
    const rpc = new WsRpc(ws);
    rpcRef.current = rpc;

    ws.onopen = () => {
      setWsStatus("connected");
      // Resize before replay so the PTY dimensions match XTerm when the
      // ring buffer snapshot is captured. WS messages are ordered, so the
      // server processes resize before replay:get — no need to await.
      ws.send(JSON.stringify({ event: "resize", cols: term.cols, rows: term.rows }));
      ws.send(JSON.stringify({ event: "replay:get", offset: 0 }));

      rpc.request({ event: "agent:get" }).then((res) => {
        if (res.ok && res.json) {
          const a = res.json as { state?: string; prompt?: PromptContext; last_message?: string };
          setPrompt(a.prompt ?? null);
          setLastMessage(a.last_message ?? null);
          if (a.state) {
            onTransitionRef.current(info.id, a.state, a.prompt ?? null, a.last_message ?? null);
          }
        }
      });
    };

    ws.onmessage = (evt) => {
      try {
        const msg = JSON.parse(evt.data);
        if (rpc.handleMessage(msg)) return;

        for (const fn of wsListenersRef.current) fn(msg as WsMessage);

        if (msg.event === "replay") {
          const bytes = b64decode(msg.data);
          harnessRef.current.replay(bytes, msg.offset, msg.next_offset);
          const action = gateRef.current.onReplay(bytes.length, msg.offset, msg.next_offset);
          if (!action) return;
          if (action.isFirst) {
            term.reset();
            handleReplayReady(term, setReady);
            // If replay didn't produce visible content (e.g. ring buffer
            // wrapped past the alt-screen-enter sequence), fall back to
            // the cached screen snapshot from the mux poller. Delay the
            // check slightly so xterm.js finishes processing the replay.
            setTimeout(() => {
              if (isTerminalBlank(term)) {
                fetchScreenFallback(info.id, term);
              }
            }, 200);
          }
          term.write(action.skip > 0 ? bytes.subarray(action.skip) : bytes);
          scheduleDiagnose();
        } else if (msg.event === "pty") {
          const bytes = b64decode(msg.data);
          harnessRef.current.pty(bytes, msg.offset);
          const skip = gateRef.current.onPty(bytes.length, msg.offset);
          if (skip === null) return;
          term.write(skip > 0 ? bytes.subarray(skip) : bytes);
          scheduleDiagnose();
        } else if (msg.event === "transition") {
          setPrompt(msg.prompt ?? null);
          setLastMessage(msg.last_message ?? null);
          onTransitionRef.current(info.id, msg.next, msg.prompt ?? null, msg.last_message ?? null);
        }
      } catch {
        // ignore parse errors
      }
    };

    ws.onclose = () => {
      if (wsRef.current === ws) wsRef.current = null;
      rpc.dispose();
      if (rpcRef.current === rpc) rpcRef.current = null;
      if (cancelledRef.current) return;
      setWsStatus("disconnected");
      // A WS upgrade that fails with 401 (expired ForwardAuth cookie) surfaces
      // here as a generic close — the browser hides the HTTP status. Probe an
      // authed endpoint; a 401 there triggers the reauth-reload in useApiClient,
      // which resolves the stranded UI. Otherwise reconnect after a short delay.
      apiGet("/api/v1/version").catch(() => {
        /* network errors are fine; only 401 matters and that's handled */
      });
      if (reconnectTimerRef.current === null) {
        reconnectTimerRef.current = setTimeout(() => {
          reconnectTimerRef.current = null;
          if (!cancelledRef.current) connectWs();
        }, 2_000);
      }
    };

    ws.onerror = () => {
      ws.close();
    };
  }

  function handleReady() {
    if (!info.webgl) {
      try {
        const webgl = new WebglAddon();
        webgl.onContextLoss(() => {
          webgl.dispose();
          if (info.webgl === webgl) info.webgl = null;
        });
        term.loadAddon(webgl);
        info.webgl = webgl;
      } catch {
        // canvas fallback
      }
    }
    fit.fit();
    connectWs();
  }

  function subscribeWsEvents(listener: WsEventListener) {
    wsListenersRef.current.add(listener);
    return () => {
      wsListenersRef.current.delete(listener);
    };
  }

  function wsSend(msg: unknown) {
    if (wsRef.current?.readyState === FallbackSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    }
  }

  function wsRequest(msg: Record<string, unknown>) {
    const rpc = rpcRef.current;
    if (!rpc) {
      return Promise.resolve({ ok: false, status: 0, json: null, text: "Not connected" } as const);
    }
    return rpc.request(msg);
  }

  function handleTerminalFocus() {
    term.focus();
  }

  return (
    <TerminalLayout
      className="absolute inset-y-0 right-0 z-[100] transition-[left] duration-200"
      style={{ left: sidebarWidth }}
      title={sessionTitle(info)}
      subtitle={[
        sessionSubtitle(info),
        info.metadata?.k8s?.runtime_class,
        formatLabels(info.metadata),
      ]
        .filter(Boolean)
        .join(" \u00b7 ")}
      credAlert={info.credAlert}
      headerRight={
        <button
          type="button"
          className="border-none bg-transparent p-1 text-zinc-500 hover:text-zinc-300"
          title="Close (Esc)"
          onClick={onClose}
        >
          <svg
            width="18"
            height="18"
            viewBox="0 0 18 18"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
          >
            <title>Close</title>
            <line x1="4" y1="4" x2="14" y2="14" />
            <line x1="14" y1="4" x2="4" y2="14" />
          </svg>
        </button>
      }
      wsStatus={wsStatus}
      agentState={info.state}
      statusLabel="[coopmux]"
      onInteraction={handleTerminalFocus}
      inspector={
        <InspectorSidebar
          subscribeWsEvents={subscribeWsEvents}
          prompt={prompt}
          lastMessage={lastMessage}
          wsSend={wsSend}
          wsRequest={wsRequest}
          onTabClick={handleTerminalFocus}
          version={version}
        />
      }
    >
      {showStreamAlert && (
        <StreamAlert message={showStreamAlert} onDismiss={() => setShowStreamAlert(null)} />
      )}
      <div className="relative min-w-0 flex-1">
        <Terminal
          instance={term}
          fitAddon={fit}
          onReady={handleReady}
          theme={THEME}
          className={`h-full py-4 pl-4 ${ready ? "" : "invisible"}`}
        />
        {!ready && (
          <div
            className="absolute inset-0 overflow-hidden"
            style={{ background: THEME.background }}
          >
            {/* Loading bar — absolutely positioned so it doesn't shift the preview */}
            <div className="absolute top-0 left-0 right-0 h-0.5 overflow-hidden bg-zinc-800">
              <div
                className="absolute inset-y-0 w-1/3 animate-[shimmer_1.5s_ease-in-out_infinite] bg-blue-500/60"
                style={{ animation: "shimmer 1.5s ease-in-out infinite" }}
              />
              <style>{`@keyframes shimmer { 0% { left: -33% } 100% { left: 100% } }`}</style>
            </div>
            {/* Cached screen preview */}
            <div className="mr-[14px] overflow-hidden py-4 pl-4">
              {info.lastScreenLines ? (
                <LoadingPreview lines={info.lastScreenLines} />
              ) : (
                <div
                  className="flex items-center gap-2 text-sm text-zinc-500"
                  style={{ fontFamily: MONO_FONT }}
                >
                  Loading session&hellip;
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    </TerminalLayout>
  );
}

/** Render cached screen lines using the shared ANSI renderer. */
function LoadingPreview({ lines }: { lines: string[] }) {
  return (
    <div
      ref={(el) => {
        if (el && !el.firstChild) {
          el.appendChild(renderAnsiPre(lines, { fontSize: EXPANDED_FONT_SIZE }));
        }
      }}
    />
  );
}

/** Build the frame for normal keystrokes / injected text.
 *
 *  Normal typing is operator-tier `input:send` on BOTH the multiplexed feed
 *  and the direct per-session WS. `input:send:raw` is reserved for admin (raw
 *  byte injection, mirroring REST `/input/raw`), so keystrokes must never use
 *  it — an edge-authed operator would get them refused. `session` is required
 *  by the mux feed's router and harmlessly ignored by the per-session upstream
 *  (it is URL-implicit there), so one frame shape serves both transports. */
export function inputSendFrame(sessionId: string, text: string) {
  return { event: "input:send", session: sessionId, text } as const;
}

/** Enable input and focus the terminal after replay completes.
 *  Focus is deferred to the next animation frame so React can first
 *  render the terminal visible (removing the `invisible` CSS class). */
export function handleReplayReady(
  term: { options: { disableStdin?: boolean }; focus: () => void },
  setReady: (v: boolean) => void,
) {
  term.options.disableStdin = false;
  setReady(true);
  requestAnimationFrame(() => term.focus());
}

/** Check if the xterm.js active buffer is visually blank (all empty lines). */
function isTerminalBlank(term: XTerm): boolean {
  const buf = term.buffer.active;
  for (let i = 0; i < buf.length; i++) {
    const line = buf.getLine(i);
    if (line && line.translateToString(true).trim() !== "") return false;
  }
  return true;
}

/** Fetch the cached screen snapshot and write it into xterm.js as a fallback.
 *  Used when PTY replay didn't produce visible content (e.g. ring buffer
 *  wrapped past the alt-screen-enter sequence for long-idle sessions). */
async function fetchScreenFallback(sessionId: string, term: XTerm) {
  const res = await apiGet(`/api/v1/sessions/${sessionId}/screen`);
  if (!res.ok || !res.json) return;
  const screen = res.json as {
    ansi?: string[];
    lines?: string[];
    alt_screen?: boolean;
    cols?: number;
    rows?: number;
  };
  const content = screen.ansi ?? screen.lines;
  if (!content || content.length === 0) return;

  // Enter alt screen if the upstream session is in alt screen mode,
  // then write the screen snapshot line by line with cursor positioning.
  if (screen.alt_screen) {
    term.write("\x1b[?1049h"); // SMCUP — enter alt screen
  }
  term.write("\x1b[H"); // cursor home
  term.write("\x1b[2J"); // clear screen
  for (let i = 0; i < content.length; i++) {
    term.write(`\x1b[${i + 1};1H`); // move cursor to row i+1, col 1
    term.write(content[i]);
  }
}
