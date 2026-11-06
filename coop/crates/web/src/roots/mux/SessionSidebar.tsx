import { useMemo, useRef, useState } from "react";
import { InfoButton, RuntimeClassChip, SessionDetails } from "@/components/SessionDetails";
import { formatLabels } from "@/components/Tile";
import type { SessionInfo } from "@/lib/types";
import { LaunchDialog } from "./LaunchDialog";
import { useMux } from "./MuxContext";

export interface SessionSidebarProps {
  sessions: SessionInfo[];
  expandedSession: string | null;
  focusedSession: string | null;
  launchAvailable: boolean;
  onSelectSession: (id: string) => void;
}

function statePriority(state: string | null): number {
  if (!state) return 5;
  const s = state.toLowerCase();
  if (s.includes("prompt")) return 0;
  if (s.includes("error") || s === "parked") return 1;
  if (s === "rate_limited") return 2;
  if (s === "idle" || s === "waiting_for_input") return 3;
  if (s === "working") return 4;
  return 5;
}

function dotColor(state: string | null): string {
  if (!state) return "bg-zinc-500";
  const s = state.toLowerCase();
  if (s.includes("prompt")) return "bg-yellow-400";
  if (s.includes("error") || s === "parked") return "bg-red-400";
  if (s === "rate_limited") return "bg-amber-400";
  if (s === "idle" || s === "waiting_for_input") return "bg-blue-400";
  if (s === "working") return "bg-green-400";
  return "bg-zinc-500";
}

function needsAttention(state: string | null): boolean {
  if (!state) return false;
  const s = state.toLowerCase();
  return s.includes("prompt") || s.includes("error");
}

function sessionLabel(info: SessionInfo): string {
  if (info.claimedBeadTitle) return info.claimedBeadTitle;
  if (info.metadata?.k8s?.pod) return info.metadata.k8s.pod;
  if (info.url) {
    try {
      return new URL(info.url).host;
    } catch {
      /* fallback */
    }
  }
  return info.id.substring(0, 12);
}

export function sortByAttention(sessions: SessionInfo[]): SessionInfo[] {
  return [...sessions].sort((a, b) => statePriority(a.state) - statePriority(b.state));
}

export function SessionSidebar({
  sessions,
  expandedSession,
  focusedSession,
  launchAvailable,
  onSelectSession,
}: SessionSidebarProps) {
  const { sidebarCollapsed: collapsed } = useMux();
  const sorted = useMemo(() => sortByAttention(sessions), [sessions]);
  const [showLaunchDialog, setShowLaunchDialog] = useState(false);
  const [detailsOpen, setDetailsOpen] = useState<Set<string>>(() => new Set());

  const toggleDetails = (id: string) => {
    setDetailsOpen((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  // Cache the most recent non-empty lastMessage per session
  const lastMessageCache = useRef<Map<string, string>>(new Map());
  for (const info of sessions) {
    if (info.lastMessage) {
      lastMessageCache.current.set(info.id, info.lastMessage);
    }
  }

  const handleLaunchClick = () => setShowLaunchDialog(true);

  if (sessions.length === 0 && !launchAvailable) return null;

  return (
    <div
      className="flex shrink-0 flex-col border-r border-[#21262d] bg-[#0d1117] transition-[width] duration-200"
      style={{ width: collapsed ? 40 : 220 }}
    >
      {/* Session list */}
      <div className="flex-1 overflow-y-auto">
        {sorted.map((info) => {
          const active = info.id === expandedSession || info.id === focusedSession;
          const pulse = needsAttention(info.state);
          const dot = dotColor(info.state);

          if (collapsed) {
            return (
              <button
                type="button"
                key={info.id}
                className={`flex w-full items-center justify-center py-2 hover:bg-[#1a1f26] ${active ? "bg-[#1a1f26]" : ""}`}
                title={`${sessionLabel(info)} \u00b7 ${(info.state || "unknown").toUpperCase()}`}
                onClick={() => onSelectSession(info.id)}
              >
                <span
                  className={`inline-block h-2.5 w-2.5 rounded-full ${dot} ${pulse ? "animate-pulse" : ""}`}
                />
              </button>
            );
          }

          const showDetails = detailsOpen.has(info.id);

          return (
            // biome-ignore lint/a11y/useSemanticElements: card contains a nested details-toggle button incompatible with <button>
            <div
              role="button"
              tabIndex={0}
              key={info.id}
              className={`flex w-full cursor-pointer items-start gap-2.5 px-3 py-2 text-left hover:bg-[#1a1f26] ${active ? "border-l-2 border-blue-500 bg-[#1a1f26]" : "border-l-2 border-transparent"}`}
              onClick={() => onSelectSession(info.id)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") onSelectSession(info.id);
              }}
            >
              <span
                className={`mt-1 inline-block h-2 w-2 shrink-0 rounded-full ${dot} ${pulse ? "animate-pulse" : ""}`}
              />
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-1.5">
                  <div className="min-w-0 flex-1 truncate text-[12px] text-zinc-300">
                    {sessionLabel(info)}
                  </div>
                  <InfoButton open={showDetails} onToggle={() => toggleDetails(info.id)} />
                </div>
                <div className="flex items-center gap-1.5">
                  <span className="text-[10px] uppercase text-zinc-500">
                    {info.state || "unknown"}
                  </span>
                  <RuntimeClassChip runtimeClass={info.metadata?.k8s?.runtime_class} />
                </div>
                {info.assignedAccount && (
                  <div
                    className={`truncate font-mono text-[10px] ${info.assignedAccount.includes("bedrock") ? "text-amber-400" : "text-green-400"}`}
                    title={info.litellmKeySuffix ? `key: ...${info.litellmKeySuffix}` : undefined}
                  >
                    {info.assignedAccount}
                    {info.litellmKeySuffix && (
                      <span className="ml-1 text-zinc-500">...{info.litellmKeySuffix}</span>
                    )}
                  </div>
                )}
                {info.totalCostUsd != null && info.totalCostUsd > 0 && (
                  <div className="font-mono text-[10px] text-emerald-400">
                    $
                    {info.totalCostUsd < 0.01
                      ? info.totalCostUsd.toFixed(4)
                      : info.totalCostUsd.toFixed(2)}
                  </div>
                )}
                {formatLabels(info.metadata) && (
                  <div className="truncate text-[10px] text-zinc-500">
                    {formatLabels(info.metadata)}
                  </div>
                )}
                {lastMessageCache.current.get(info.id) && (
                  <div className="mt-0.5 line-clamp-2 text-[10px] leading-tight text-zinc-500">
                    {lastMessageCache.current.get(info.id)}
                  </div>
                )}
                {showDetails && (
                  <SessionDetails info={info} className="mt-1.5 border-t border-[#21262d] pt-1.5" />
                )}
              </div>
            </div>
          );
        })}
      </div>

      {/* New session button */}
      {launchAvailable &&
        (collapsed ? (
          <button
            type="button"
            className="flex w-full items-center justify-center border-t border-[#21262d] py-2 text-zinc-500 hover:bg-[#1a1f26] hover:text-blue-400"
            onClick={handleLaunchClick}
            title="New session"
          >
            <span className="text-base leading-none">+</span>
          </button>
        ) : (
          <button
            type="button"
            className="flex w-full items-center gap-2 border-t border-[#21262d] px-3 py-2 text-[12px] text-zinc-500 hover:bg-[#1a1f26] hover:text-blue-400"
            onClick={handleLaunchClick}
          >
            <span className="text-base leading-none">+</span>
            <span>New Session</span>
          </button>
        ))}

      {/* Launch dialog */}
      {showLaunchDialog && <LaunchDialog onClose={() => setShowLaunchDialog(false)} />}
    </div>
  );
}
