import { useEffect, useRef, useState } from "react";
import { DropOverlay } from "@/components/DropOverlay";
import { OAuthToast } from "@/components/OAuthToast";
import { StatusBar } from "@/components/StatusBar";
import { LaunchCard, Tile } from "@/components/Tile";
import { apiGet } from "@/hooks/useApiClient";
import { useFileUpload } from "@/hooks/useFileUpload";
import { useWebSocket } from "@/hooks/useWebSocket";
import { useInit } from "@/hooks/utils";
import type { MuxMetadata, MuxWsMessage, PromptContext, SessionInfo } from "@/lib/types";
import { CredentialPanel } from "./CredentialPanel";
import { ExpandedSession } from "./ExpandedSession";
import { MuxProvider, useMux } from "./MuxContext";
import { SessionSidebar } from "./SessionSidebar";

function AppInner() {
  const [sessions, setSessions] = useState<Map<string, SessionInfo>>(() => new Map());
  const sessionsRef = useRef(sessions);
  sessionsRef.current = sessions;

  const [focusedSession, setFocusedSession] = useState<string | null>(null);
  const focusedRef = useRef(focusedSession);
  focusedRef.current = focusedSession;

  const [expandedSession, setExpandedSession] = useState<string | null>(null);
  const expandedRef = useRef(expandedSession);
  expandedRef.current = expandedSession;

  const expandedSendInputRef = useRef<((text: string) => void) | null>(null);

  const [launchAvailable, setLaunchAvailable] = useState(false);
  const [oauthUrl, setOauthUrl] = useState<string | null>(null);

  const [credPanelOpen, setCredPanelOpen] = useState(false);

  const [version, setVersion] = useState<string | null>(null);

  // The header account indicator below is derived from the live per-session
  // accounts, NOT from a pool heuristic. The old code picked the healthy
  // account with the most sessions, which has no relationship to the
  // credential any visible pod is using — and degenerated to "first account in
  // the list" whenever pool session counts were all zero. See ka-0000002509.
  // (The Credentials panel fetches its own pool view when opened.)

  // Distinct accounts actually in use across the live sessions, derived from
  // each session's reconciled assignedAccount (the credential the pod is really
  // running on). This is the accurate fleet-level account view: one chip when
  // every session shares an account, or a count when they differ.
  const accountsInUse = new Set<string>();
  for (const [, info] of sessions) {
    if (info.assignedAccount) accountsInUse.add(info.assignedAccount);
  }
  const accountList = [...accountsInUse].sort();

  // Stats
  const sessionCount = sessions.size;
  let healthyCount = 0;
  for (const [, info] of sessions) {
    const s = (info.state || "").toLowerCase();
    if (s && s !== "exited" && !s.includes("error")) healthyCount++;
  }

  function createSession(
    id: string,
    url: string | null,
    state: string | null,
    metadata: MuxMetadata | null,
    assignedAccount?: string | null,
    litellmKeySuffix?: string | null,
    claimedBeadTitle?: string | null,
    totalCostUsd?: number | null,
  ): SessionInfo {
    return {
      id,
      url,
      state,
      metadata,
      lastMessage: null,
      term: null,
      fit: null,
      webgl: null,
      sourceCols: 80,
      sourceRows: 24,
      lastScreenLines: null,
      credAlert: false,
      assignedAccount: assignedAccount ?? null,
      litellmKeySuffix: litellmKeySuffix ?? null,
      claimedBeadTitle: claimedBeadTitle ?? null,
      totalCostUsd: totalCostUsd ?? null,
    };
  }

  function handleExpandedTransition(
    sessionId: string,
    next: string,
    prompt: PromptContext | null,
    _lastMessage: string | null,
  ) {
    const info = sessionsRef.current.get(sessionId);
    if (info) {
      info.state = next;
      setSessions(new Map(sessionsRef.current));
    }
    if (prompt?.subtype === "oauth_login" && prompt.input) {
      setOauthUrl(prompt.input);
    }
  }

  function toggleExpand(id: string) {
    if (expandedRef.current === id) {
      setExpandedSession(null);
      history.replaceState(null, "", location.pathname + location.search);
    } else {
      setExpandedSession(id);
      setFocusedSession(id);
      history.replaceState(null, "", `${location.pathname}${location.search}#${id}`);
    }
  }

  // Deep-link: read session ID from URL hash fragment (e.g. #pod-name).
  // Set on mount and updated on hashchange events (e.g. clicking a Slack
  // link while the page is already open). Consumed when the target session
  // becomes available, then cleared.
  const deepLinkRef = useRef<string | null>(location.hash.replace(/^#/, "") || null);

  // Listen for hash changes so that clicking a coopmux link while the
  // dashboard is already open expands the targeted session.
  useEffect(() => {
    const onHashChange = () => {
      const target = location.hash.replace(/^#/, "") || null;
      if (!target) return;
      if (sessionsRef.current.has(target)) {
        setExpandedSession(target);
        setFocusedSession(target);
      } else {
        // Session not yet known — stash for consumption when it arrives.
        deepLinkRef.current = target;
      }
    };
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  const muxSendRef = useRef<((msg: unknown) => void) | null>(null);

  function onMuxMessage(raw: unknown) {
    const msg = raw as MuxWsMessage;

    if (msg.event === "sessions") {
      const newSessions = new Map<string, SessionInfo>();
      const ids: string[] = [];
      for (const s of msg.sessions) {
        ids.push(s.id);
        if (sessionsRef.current.has(s.id)) {
          // Reuse existing SessionInfo (preserves state + screen data)
          const existing = sessionsRef.current.get(s.id)!;
          // Update URL/state/metadata from backend (in case they changed)
          existing.url = s.url ?? null;
          existing.state = s.state ?? null;
          existing.metadata = s.metadata ?? null;
          existing.assignedAccount = s.assigned_account ?? existing.assignedAccount;
          existing.litellmKeySuffix = s.litellm_key_suffix ?? existing.litellmKeySuffix;
          existing.claimedBeadTitle = s.claimed_bead_title ?? existing.claimedBeadTitle;
          if (s.total_cost_usd != null) existing.totalCostUsd = s.total_cost_usd;
          newSessions.set(s.id, existing);
        } else {
          newSessions.set(
            s.id,
            createSession(
              s.id,
              s.url ?? null,
              s.state ?? null,
              s.metadata ?? null,
              s.assigned_account,
              s.litellm_key_suffix,
              s.claimed_bead_title,
              s.total_cost_usd,
            ),
          );
        }
      }
      // Dispose terminals for sessions that are no longer in the backend list
      for (const [id, info] of sessionsRef.current) {
        if (!newSessions.has(id)) {
          info.term?.dispose();
        }
      }
      sessionsRef.current = newSessions;
      setSessions(newSessions);
      if (ids.length > 0 && muxSendRef.current) {
        muxSendRef.current({ event: "subscribe", sessions: ids });
      }
      // Auto-expand session from URL hash deep-link (consumed once).
      const target = deepLinkRef.current;
      if (target && newSessions.has(target)) {
        deepLinkRef.current = null;
        history.replaceState(null, "", location.pathname + location.search);
        setExpandedSession(target);
        setFocusedSession(target);
      }
    } else if (msg.event === "transition") {
      const info = sessionsRef.current.get(msg.session);
      if (info) {
        // When a session goes idle/waiting_for_input due to a rate-limit or
        // budget signal, surface it as a distinct "rate_limited" UI state so
        // operators can see throttled sessions at a glance.
        const isThrottled =
          (msg.next === "idle" || msg.next === "waiting_for_input") &&
          (msg.error_category === "rate_limited" || msg.error_category === "budget_exceeded");
        info.state = isThrottled ? "rate_limited" : msg.next;
        if (msg.last_message != null) info.lastMessage = msg.last_message;
        setSessions(new Map(sessionsRef.current));
      }
      if (msg.prompt?.subtype === "oauth_login" && msg.prompt.input) {
        setOauthUrl(msg.prompt.input);
      }
    } else if (msg.event === "session:online") {
      if (!sessionsRef.current.has(msg.session)) {
        const newSessions = new Map(sessionsRef.current);
        newSessions.set(
          msg.session,
          createSession(msg.session, msg.url ?? null, null, msg.metadata ?? null),
        );
        sessionsRef.current = newSessions;
        setSessions(newSessions);
        muxSendRef.current?.({ event: "subscribe", sessions: [msg.session] });
        // Auto-expand if this is the deep-link target arriving late.
        const target = deepLinkRef.current;
        if (target && target === msg.session) {
          deepLinkRef.current = null;
          history.replaceState(null, "", location.pathname + location.search);
          setExpandedSession(target);
          setFocusedSession(target);
        }
      }
    } else if (msg.event === "session:offline") {
      const info = sessionsRef.current.get(msg.session);
      if (info) {
        info.term?.dispose();
        const newSessions = new Map(sessionsRef.current);
        newSessions.delete(msg.session);
        sessionsRef.current = newSessions;
        setSessions(newSessions);
        if (focusedRef.current === msg.session) setFocusedSession(null);
        if (expandedRef.current === msg.session) setExpandedSession(null);
      }
    } else if (msg.event === "screen_batch") {
      for (const scr of msg.screens) {
        const info = sessionsRef.current.get(scr.session);
        if (!info) continue;

        const lines = scr.lines.slice();
        const ansi = scr.ansi?.slice() ?? lines.slice();
        // Trim trailing blank lines, but leave one for bottom padding.
        while (lines.length > 1 && lines[lines.length - 1].trim() === "") {
          lines.pop();
          ansi.pop();
        }

        info.sourceCols = scr.cols;
        info.sourceRows = scr.rows;
        info.lastScreenLines = ansi;
        if (scr.state != null) info.state = scr.state;
        if (scr.assigned_account != null) info.assignedAccount = scr.assigned_account;
        if (scr.litellm_key_suffix != null) info.litellmKeySuffix = scr.litellm_key_suffix;
        if (scr.total_cost_usd != null) info.totalCostUsd = scr.total_cost_usd;
      }
      setSessions((prev) => new Map(prev));
    }
  }

  const { send: muxSend, status: muxWsStatus } = useWebSocket({
    path: "/ws/mux",
    onMessage: onMuxMessage,
  });

  // Keep muxSendRef in sync
  muxSendRef.current = muxSend;

  useInit(() => {
    apiGet("/api/v1/config/launch").then((res) => {
      if (
        res.ok &&
        res.json &&
        typeof res.json === "object" &&
        "available" in (res.json as Record<string, unknown>)
      ) {
        setLaunchAvailable((res.json as Record<string, unknown>).available === true);
      }
    });
    apiGet("/api/v1/version").then((res) => {
      if (res.ok && res.json && typeof res.json === "object") {
        const v = (res.json as Record<string, unknown>).version;
        if (typeof v === "string") setVersion(v);
      }
    });
  });

  const { dragActive } = useFileUpload({
    uploadPath: () => (focusedRef.current ? `/api/v1/sessions/${focusedRef.current}/upload` : null),
    onUploaded: (paths) => {
      const text = `${paths.join(" ")} `;
      const focused = focusedRef.current;
      if (!focused) return;
      if (expandedSendInputRef.current) {
        expandedSendInputRef.current(text);
      } else {
        muxSendRef.current?.({ event: "input:send", session: focused, text });
      }
      sessionsRef.current.get(focused)?.term?.focus();
    },
    onError: (msg) => {
      const focused = focusedRef.current;
      if (focused) {
        const info = sessionsRef.current.get(focused);
        info?.term?.write(`\r\n\x1b[31m[${msg}]\x1b[0m\r\n`);
      }
    },
  });

  const { sidebarCollapsed, toggleSidebar } = useMux();
  const sidebarWidth = sidebarCollapsed ? 40 : 220;

  const credPanelOpenRef = useRef(credPanelOpen);
  credPanelOpenRef.current = credPanelOpen;
  const toggleSidebarRef = useRef(toggleSidebar);
  toggleSidebarRef.current = toggleSidebar;
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        if (credPanelOpenRef.current) {
          setCredPanelOpen(false);
        } else if (expandedRef.current) {
          setExpandedSession(null);
        }
      }
      if (e.key === "b" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        toggleSidebarRef.current();
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, []);

  const sessionArray = [...sessions.values()];

  // Aggregate cost across all sessions
  let totalCost = 0;
  for (const info of sessionArray) {
    if (info.totalCostUsd != null) totalCost += info.totalCostUsd;
  }

  return (
    <div className="flex h-screen flex-col bg-[#0d1117] font-sans text-[#c9d1d9]">
      {/* Header */}
      <header className="flex shrink-0 items-center gap-4 border-b border-[#21262d] px-2.5 py-2.5">
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="border-none bg-transparent p-0.5 text-zinc-500 hover:text-zinc-300"
            onClick={toggleSidebar}
            title={sidebarCollapsed ? "Expand sidebar (Cmd+B)" : "Collapse sidebar (Cmd+B)"}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <title>Toggle sidebar</title>
              <rect x="1.5" y="2" width="13" height="12" rx="1.5" />
              <line x1="5.5" y1="2" x2="5.5" y2="14" />
            </svg>
          </button>
          <h1 className="text-base font-semibold">coopmux</h1>
        </div>
        <div className="flex gap-4 text-[13px] text-zinc-500">
          <span>
            {sessionCount} session{sessionCount !== 1 ? "s" : ""}
          </span>
          <span>{healthyCount} healthy</span>
          {totalCost > 0 && (
            <span className="font-mono text-emerald-400" title="Total spend across all sessions">
              ${totalCost < 0.01 ? totalCost.toFixed(4) : totalCost.toFixed(2)}
            </span>
          )}
        </div>
        {accountList.length > 0 && (
          <div
            className="flex items-center gap-1.5 text-[12px]"
            title={
              accountList.length === 1
                ? `All sessions on account: ${accountList[0]}`
                : `Accounts in use: ${accountList.join(", ")}`
            }
          >
            <span className="inline-block h-2 w-2 rounded-full bg-green-500" />
            <span className="font-mono text-zinc-300">
              {accountList.length === 1
                ? accountList[0]
                : `${accountList.length} accounts`}
            </span>
          </div>
        )}
        <div className="relative ml-auto">
          <button
            type="button"
            className="rounded border border-zinc-700 bg-[#1c2128] px-2.5 py-1 text-[12px] text-zinc-400 transition-colors hover:border-zinc-500 hover:text-zinc-300"
            onClick={() => setCredPanelOpen((v) => !v)}
          >
            Credentials
          </button>
          {credPanelOpen && <CredentialPanel onClose={() => setCredPanelOpen(false)} />}
        </div>
      </header>

      <DropOverlay active={dragActive} />
      {oauthUrl && <OAuthToast url={oauthUrl} onDismiss={() => setOauthUrl(null)} />}

      {/* Main area: sidebar + content */}
      <div className="relative flex min-h-0 flex-1 flex-col">
        <div className="flex min-h-0 flex-1">
          <SessionSidebar
            sessions={sessionArray}
            expandedSession={expandedSession}
            focusedSession={focusedSession}
            launchAvailable={launchAvailable}
            onSelectSession={(id) => toggleExpand(id)}
          />

          {/* Grid */}
          {sessionCount > 0 || launchAvailable ? (
            <div className="grid flex-1 auto-rows-min grid-cols-[repeat(auto-fill,minmax(340px,1fr))] content-start gap-3 overflow-auto p-4">
              {sessionArray
                .filter((info) => info.id !== expandedSession)
                .map((info) => (
                  <Tile
                    key={info.id}
                    info={info}
                    focused={focusedSession === info.id}
                    onToggleExpand={() => toggleExpand(info.id)}
                  />
                ))}
              {launchAvailable && <LaunchCard />}
            </div>
          ) : (
            <div className="flex flex-1 items-center justify-center text-sm text-zinc-500">
              <p>Waiting for connections&hellip;</p>
            </div>
          )}
        </div>

        {/* Expanded session overlay */}
        {expandedSession && sessions.get(expandedSession) && (
          <ExpandedSession
            key={expandedSession}
            info={sessions.get(expandedSession)!}
            sidebarWidth={sidebarWidth}
            muxSend={muxSend}
            sendInputRef={expandedSendInputRef}
            onTransition={handleExpandedTransition}
            onClose={() => setExpandedSession(null)}
            version={version ?? undefined}
          />
        )}

        {/* Page-level status bar */}
        <StatusBar label="[coopmux]" wsStatus={muxWsStatus} version={version ?? undefined} />
      </div>
    </div>
  );
}

export function App() {
  return (
    <MuxProvider>
      <AppInner />
    </MuxProvider>
  );
}
