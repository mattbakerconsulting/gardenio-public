// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import type { SessionInfo } from "@/lib/types";

/** Chip styling for a K8s runtime class. gVisor (runsc) gets a distinct
 * accent so sandboxed agents stand out; anything else gets a neutral chip. */
export function runtimeClassChipClass(runtimeClass: string): string {
  if (runtimeClass === "runsc") return "bg-purple-500/20 text-purple-300";
  return "bg-zinc-700 text-zinc-300";
}

export function RuntimeClassChip({
  runtimeClass,
  className,
}: {
  runtimeClass: string | undefined;
  className?: string;
}) {
  if (!runtimeClass) return null;
  return (
    <span
      className={`inline-block rounded px-1 py-px font-mono text-[9px] leading-tight ${runtimeClassChipClass(runtimeClass)} ${className || ""}`}
      title={`K8s runtime class: ${runtimeClass}${runtimeClass === "runsc" ? " (gVisor sandbox)" : ""}`}
    >
      {runtimeClass}
    </span>
  );
}

/** Flatten a SessionInfo into labeled detail rows for the info panel.
 * Empty/unknown values are omitted, except runtime class which always shows
 * ("default" when the pod runs on the cluster default runtime). */
export function detailRows(info: SessionInfo): { label: string; value: string }[] {
  const k8s = info.metadata?.k8s;
  const rows: { label: string; value: string | null | undefined }[] = [
    { label: "session", value: info.id },
    { label: "state", value: info.state },
    { label: "agent", value: info.metadata?.agent },
    { label: "runtime", value: k8s ? (k8s.runtime_class ?? "default") : null },
    { label: "pod", value: k8s?.pod },
    { label: "namespace", value: k8s?.namespace },
    { label: "node", value: k8s?.node },
    { label: "ip", value: k8s?.ip },
    { label: "svc account", value: k8s?.service_account },
    { label: "project", value: info.metadata?.project },
    { label: "role", value: info.metadata?.role },
    { label: "agent bead", value: info.metadata?.agent_bead_id },
    { label: "account", value: info.assignedAccount },
    { label: "key", value: info.litellmKeySuffix ? `...${info.litellmKeySuffix}` : null },
    {
      label: "cost",
      value:
        info.totalCostUsd != null && info.totalCostUsd > 0
          ? `$${info.totalCostUsd < 0.01 ? info.totalCostUsd.toFixed(4) : info.totalCostUsd.toFixed(2)}`
          : null,
    },
    { label: "task", value: info.claimedBeadTitle },
    { label: "url", value: info.url },
  ];
  return rows.filter((r): r is { label: string; value: string } => !!r.value);
}

/** Key-value detail table for a session — rendered when the ⓘ button on a
 * card is toggled open. */
export function SessionDetails({ info, className }: { info: SessionInfo; className?: string }) {
  return (
    <div className={`grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 ${className || ""}`}>
      {detailRows(info).map((row) => (
        <div key={row.label} className="contents">
          <span className="text-[10px] uppercase text-zinc-500">{row.label}</span>
          <span className="min-w-0 break-all font-mono text-[10px] text-zinc-300" title={row.value}>
            {row.value}
          </span>
        </div>
      ))}
    </div>
  );
}

/** Small ⓘ toggle button shared by the sidebar cards and grid tiles. */
export function InfoButton({
  open,
  onToggle,
  className,
}: {
  open: boolean;
  onToggle: () => void;
  className?: string;
}) {
  return (
    <button
      type="button"
      className={`flex h-4 w-4 shrink-0 items-center justify-center rounded-full border text-[9px] leading-none transition-colors ${open ? "border-blue-500 bg-blue-500/20 text-blue-400" : "border-zinc-600 text-zinc-500 hover:border-zinc-400 hover:text-zinc-300"} ${className || ""}`}
      title={open ? "Hide details" : "Show details"}
      onClick={(e) => {
        e.stopPropagation();
        onToggle();
      }}
      onKeyDown={(e) => {
        // Keep Enter/Space from also triggering the surrounding card.
        e.stopPropagation();
      }}
    >
      i
    </button>
  );
}
