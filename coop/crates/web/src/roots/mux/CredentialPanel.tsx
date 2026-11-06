import { useEffect, useRef, useState } from "react";
import { ActionBtn } from "@/components/ActionBtn";
import { ResultDisplay, showResult } from "@/components/ResultDisplay";
import { Section } from "@/components/Section";
import { apiGet, apiPost } from "@/hooks/useApiClient";
import { useInterval } from "@/hooks/utils";

interface AccountStatus {
  name: string;
  provider: string;
  status: "healthy" | "missing";
  has_api_key: boolean;
}

interface PoolAccount {
  name: string;
  provider: string;
  status: string;
  session_count: number;
  cumulative_assignments: number;
  has_api_key: boolean;
  // Rate-limit probe fields (from controller ratelimit_probe; empty if not yet probed)
  throttle_status?: string;
  utilization_5h?: string;
  utilization_7d?: string;
  throttle_reset_at?: string;
  representative_claim?: string;
}

interface PoolSummary {
  strategy?: "least_loaded" | "round_robin";
  rotator_cursor?: number | null;
  rotator_next_index?: number | null;
  // When auto-assign is disabled (COOPMUX_DISABLE_AUTO_ASSIGN=1, the prod
  // default), the broker never tracks session assignments, so session_count /
  // cumulative_assignments are stuck at 0 and Rebalance / Push are no-ops. Hide
  // those misleading counts + controls in that mode. See ka-0000005557.
  auto_assign_disabled?: boolean;
}

const statusStyles: Record<string, string> = {
  healthy: "bg-green-500/20 text-green-400",
  missing: "bg-yellow-500/20 text-yellow-400",
  throttled: "bg-red-500/20 text-red-400",
};

function StatusBadge({ status }: { status: string }) {
  return (
    <span
      className={`inline-block rounded-full px-2 py-0.5 text-[10px] font-medium uppercase ${statusStyles[status] || "bg-zinc-700 text-zinc-400"}`}
    >
      {status}
    </span>
  );
}

function UtilBar({ label, value }: { label: string; value: string }) {
  const pct = Math.round(parseFloat(value || "0") * 100);
  if (!value) return null;
  const color = pct >= 90 ? "bg-red-500" : pct >= 70 ? "bg-yellow-500" : "bg-green-500";
  return (
    <div className="mt-0.5 flex items-center gap-1">
      <span className="w-5 shrink-0 text-[9px] text-zinc-500">{label}</span>
      <div className="h-1 flex-1 overflow-hidden rounded bg-zinc-700">
        <div className={`h-full ${color}`} style={{ width: `${pct}%` }} />
      </div>
      <span className="w-7 shrink-0 text-right text-[9px] text-zinc-400">{pct}%</span>
    </div>
  );
}

/** Env key options per provider. First entry is the default. */
const providerEnvKeys: Record<string, string[]> = {
  claude: ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"],
  openai: ["OPENAI_API_KEY"],
  gemini: ["GEMINI_API_KEY"],
  other: ["API_KEY"],
};

function envKeysForProvider(provider: string): string[] {
  return providerEnvKeys[provider] ?? ["API_KEY"];
}

interface CredentialPanelProps {
  onClose: () => void;
}

export function CredentialPanel({ onClose }: CredentialPanelProps) {
  const [accounts, setAccounts] = useState<AccountStatus[]>([]);
  const [pool, setPool] = useState<PoolAccount[]>([]);
  const [poolSummary, setPoolSummary] = useState<PoolSummary>({});
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const [showForm, setShowForm] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);

  async function fetchStatus() {
    const res = await apiGet("/api/v1/credentials/status");
    if (res.ok && Array.isArray(res.json)) {
      setAccounts(res.json as AccountStatus[]);
    }
  }

  async function fetchPool() {
    const res = await apiGet("/api/v1/credentials/pool");
    if (res.ok && res.json && typeof res.json === "object") {
      const data = res.json as {
        accounts?: PoolAccount[];
        strategy?: "least_loaded" | "round_robin";
        rotator_cursor?: number | null;
        rotator_next_index?: number | null;
        auto_assign_disabled?: boolean;
      };
      if (Array.isArray(data.accounts)) {
        setPool(data.accounts);
      }
      setPoolSummary({
        strategy: data.strategy,
        rotator_cursor: data.rotator_cursor ?? null,
        rotator_next_index: data.rotator_next_index ?? null,
        auto_assign_disabled: data.auto_assign_disabled ?? false,
      });
    }
  }

  useInterval(() => {
    fetchStatus();
    fetchPool();
  }, 10_000);

  // Click outside closes panel.
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (panelRef.current && !panelRef.current.contains(e.target as Node)) {
        onCloseRef.current();
      }
    };
    document.addEventListener("mousedown", onClick);
    return () => document.removeEventListener("mousedown", onClick);
  }, []);

  async function handleDistribute(account: string) {
    setResult(null);
    const res = await apiPost("/api/v1/credentials/distribute", { account, switch: true });
    setResult(showResult(res));
  }

  async function handleRebalance() {
    setResult(null);
    const res = await apiPost("/api/v1/credentials/pool/rebalance", {});
    setResult(showResult(res));
    fetchPool();
  }

  const [formName, setFormName] = useState("");
  const [formProvider, setFormProvider] = useState("claude");
  const [formEnvKey, setFormEnvKey] = useState(envKeysForProvider("claude")[0]);
  const [formToken, setFormToken] = useState("");
  const [formSubmitting, setFormSubmitting] = useState(false);

  async function handleAddAccount() {
    if (!formName.trim()) return;
    setFormSubmitting(true);
    setResult(null);

    const body: Record<string, unknown> = {
      name: formName.trim(),
      provider: formProvider,
      env_key: formEnvKey,
    };
    if (formToken.trim()) {
      body.token = formToken.trim();
    }

    const res = await apiPost("/api/v1/credentials/new", body);
    setResult(showResult(res));
    setFormSubmitting(false);

    if (res.ok) {
      setFormName("");
      setFormToken("");
      setFormEnvKey(envKeysForProvider(formProvider)[0]);
      setShowForm(false);
      fetchStatus();
      fetchPool();
    }
  }

  const totalSessions = pool.reduce((sum, a) => sum + a.session_count, 0);
  const healthyAccounts = pool.filter((a) => a.status === "healthy").length;
  // In prod, auto-assign is disabled, so the broker never tracks session
  // assignments: per-account session_count / cumulative_assignments are stuck
  // at 0 and Rebalance / Push are no-ops. Hide those misleading counts +
  // controls rather than show fabricated zeros. See ka-0000005557.
  const autoAssignDisabled = poolSummary.auto_assign_disabled ?? false;

  return (
    <div
      ref={panelRef}
      className="absolute right-0 top-full z-[200] mt-1 w-80 rounded border border-[#21262d] bg-[#161b22] shadow-xl"
    >
      {/* Pool summary */}
      {pool.length > 0 && (
        <Section
          label="Pool"
          headerRight={
            autoAssignDisabled ? undefined : (
              <button
                type="button"
                className="text-[10px] text-zinc-500 hover:text-zinc-300"
                onClick={handleRebalance}
              >
                Rebalance
              </button>
            )
          }
        >
          <div className="mb-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[10px] text-zinc-400">
            <span>
              {healthyAccounts}/{pool.length} healthy
            </span>
            {!autoAssignDisabled && <span>{totalSessions} sessions</span>}
            {poolSummary.strategy && (
              <span className="font-mono text-zinc-500">
                strategy: <span className="text-zinc-300">{poolSummary.strategy}</span>
              </span>
            )}
            {poolSummary.strategy === "round_robin" && poolSummary.rotator_cursor !== null && (
              <span className="font-mono text-zinc-500">
                cursor: <span className="text-zinc-300">{poolSummary.rotator_cursor}</span>
                {poolSummary.rotator_next_index !== null &&
                  poolSummary.rotator_next_index !== undefined && (
                    <span className="text-zinc-500">
                      {" "}
                      (next {poolSummary.rotator_next_index}/{healthyAccounts})
                    </span>
                  )}
              </span>
            )}
          </div>
          {autoAssignDisabled && (
            <div className="mb-1 text-[9px] leading-tight text-zinc-500">
              Auto-assign disabled — sessions run on the credential injected at spawn. Per-account
              session counts and rebalancing are not tracked.
            </div>
          )}
          {pool.map((acct) => {
            const isThrottled = acct.throttle_status === "rejected";
            return (
              <div key={acct.name} className="mb-1 rounded bg-[#1c2128] px-2 py-1">
                <div className="flex items-center gap-2">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-1.5">
                      <span className="truncate text-[11px] font-mono text-zinc-300">
                        {acct.name}
                      </span>
                      <StatusBadge status={isThrottled ? "throttled" : acct.status} />
                    </div>
                  </div>
                  {!autoAssignDisabled && (
                    <span
                      className="shrink-0 text-[10px] text-zinc-500"
                      title={`${acct.cumulative_assignments} lifetime assignments`}
                    >
                      {acct.session_count} sess · {acct.cumulative_assignments} total
                    </span>
                  )}
                  {!autoAssignDisabled && acct.status === "healthy" && !isThrottled && (
                    <ActionBtn onClick={() => handleDistribute(acct.name)}>Push</ActionBtn>
                  )}
                </div>
                {(acct.utilization_5h || acct.utilization_7d) && (
                  <div className="mt-0.5">
                    <UtilBar label="5h" value={acct.utilization_5h ?? ""} />
                    <UtilBar label="7d" value={acct.utilization_7d ?? ""} />
                    {isThrottled && acct.throttle_reset_at && (
                      <div className="mt-0.5 text-[9px] text-red-400">
                        resets {acct.throttle_reset_at.replace("T", " ").slice(0, 16)}Z
                      </div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </Section>
      )}

      {/* Account list */}
      <Section
        label="Accounts"
        headerRight={
          <button
            type="button"
            className="text-[10px] text-zinc-500 hover:text-zinc-300"
            onClick={() => setShowForm((v) => !v)}
          >
            {showForm ? "Cancel" : "+ Add"}
          </button>
        }
      >
        {accounts.length === 0 && !showForm && (
          <div className="py-2 text-center text-[11px] text-zinc-500">No accounts configured</div>
        )}
        {accounts.map((acct) => (
          <div
            key={acct.name}
            className="mb-1.5 flex items-center gap-2 rounded bg-[#1c2128] px-2 py-1.5"
          >
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-1.5">
                <span className="truncate text-[11px] font-mono text-zinc-300">{acct.name}</span>
                <span className="shrink-0 rounded bg-[#2a2a2a] px-1 py-px text-[9px] uppercase text-zinc-500">
                  {acct.provider}
                </span>
              </div>
              <div className="flex items-center gap-1.5 mt-0.5">
                <StatusBadge status={acct.status} />
              </div>
            </div>
          </div>
        ))}

        {/* Add account form */}
        {showForm && (
          <div className="mt-2 rounded border border-[#2a2a2a] bg-[#1c2128] p-2">
            <div className="mb-1.5 flex gap-2">
              <input
                className="flex-1 rounded border border-[#2a2a2a] bg-[#0d1117] px-2 py-1 text-[11px] font-mono text-zinc-300 placeholder-zinc-600 outline-none focus:border-zinc-500"
                placeholder="Account name"
                value={formName}
                onChange={(e) => setFormName(e.target.value)}
              />
              <select
                className="rounded border border-[#2a2a2a] bg-[#0d1117] px-1.5 py-1 text-[11px] font-mono text-zinc-300 outline-none"
                value={formProvider}
                onChange={(e) => {
                  setFormProvider(e.target.value);
                  setFormEnvKey(envKeysForProvider(e.target.value)[0]);
                }}
              >
                <option value="claude">claude</option>
                <option value="openai">openai</option>
                <option value="gemini">gemini</option>
                <option value="other">other</option>
              </select>
            </div>
            <select
              className="mb-1.5 w-full rounded border border-[#2a2a2a] bg-[#0d1117] px-2 py-1 text-[11px] font-mono text-zinc-300 outline-none"
              value={formEnvKey}
              onChange={(e) => setFormEnvKey(e.target.value)}
            >
              {envKeysForProvider(formProvider).map((key) => (
                <option key={key} value={key}>
                  {key}
                </option>
              ))}
            </select>
            <input
              className="mb-1.5 w-full rounded border border-[#2a2a2a] bg-[#0d1117] px-2 py-1 text-[11px] font-mono text-zinc-300 placeholder-zinc-600 outline-none focus:border-zinc-500"
              placeholder="Token (optional)"
              type="password"
              value={formToken}
              onChange={(e) => setFormToken(e.target.value)}
            />
            <ActionBtn
              variant="success"
              onClick={handleAddAccount}
              className={formSubmitting ? "opacity-50" : ""}
            >
              {formSubmitting ? "Adding..." : "Add Account"}
            </ActionBtn>
          </div>
        )}
      </Section>

      <ResultDisplay result={result} />
    </div>
  );
}
