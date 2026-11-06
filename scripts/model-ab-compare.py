#!/usr/bin/env python3
"""model-ab-compare.py — parallel Haiku vs Opus-5 cost/latency A-B harness.

Task ka-0000007402 (epic ka-0000000949, "Opus 5 cutover + model-switch/refusal
observability", task 1). Answers ONE question the fleet cannot answer today:

    For a fixed, representative task corpus, what is the per-model tradeoff in
    COST (real LiteLLM spend) and LATENCY (turn wall-clock) between
    claude-haiku-4-5 and claude-opus-5?

WHY THIS IS A DISTINCT TOOL (not scripts/opus5-ab.py)
  scripts/opus5-ab.py is a CONTEXT-audit A-B: it compares fleet behaviour
  BEFORE vs AFTER a context trim, sampled from live agent beads. It never runs
  a task through two DIFFERENT models. This harness is the cross-MODEL
  comparator that gap left open: it drives the SAME corpus through EACH model
  and measures the delta directly.

HOW IT MEASURES (two independent signals, kept explicit — advice ka-0000005924)
  (1) LATENCY  — wall-clock of each /v1/messages call, measured locally by this
      harness. One call == one turn. Reported as a per-model distribution
      (mean / p50 / p90 / max) over corpus x --repeat turns.
  (2) COST     — the LiteLLM-computed spend for each call, read from the
      `x-litellm-response-cost` response header (LiteLLM's own price map applied
      to the call). Summed per model → authoritative per-model spend. Raw token
      usage is recorded alongside so cost is reconstructible and cross-checkable.
      When a master key is available, --verify-key-spend cross-checks the summed
      header cost against LiteLLM /key/info for the scoped key (a second,
      independent derivation of the same dollars).

CREDENTIALS — the SAFE default needs NO master key
  The measurement path needs only LITELLM_URL + one SCOPED virtual key per model
  (the key computes cost via the response header — no admin access required).
  Provide scoped keys per model via:
      --key-<model>=sk-...            (repeatable; model name sanitized, see below)
      or env LITELLM_KEY_<SANITIZED>  (e.g. LITELLM_KEY_CLAUDE_HAIKU_4_5)
  The master key is a controller/operator credential and MUST NOT be placed on
  the agent path (docs/claudemore-per-ci-budget-key.md). --mint is an OPTIONAL
  convenience: when LITELLM_MASTER_KEY is set it mints a budget-capped, model-
  scoped virtual key per model (POST /key/generate) and revokes it at teardown
  (POST /key/delete) — the same mint→run→delete lifecycle as
  tests/e2e/scripts/test-litellm-budget-cap.sh. Use --mint only where the master
  key legitimately lives (a gated operator-run lane), never in an agent pod.

BOUNDED BY CONSTRUCTION
  Total live calls == len(corpus) * len(models) * --repeat, a fixed small number.
  Minted keys carry --max-budget so even a pathological proxy cannot overspend.
  A per-model call cap is enforced regardless of corpus size. --dry-run makes
  the whole plan (models, keys resolved, call count, budget) with ZERO network.

USAGE
  # measure with pre-provisioned scoped keys (safe default, no master key):
  LITELLM_URL=https://litellm... \
  LITELLM_KEY_CLAUDE_HAIKU_4_5=sk-... LITELLM_KEY_CLAUDE_OPUS_5=sk-... \
    python3 scripts/model-ab-compare.py \
      --corpus scripts/model-ab-corpus.json --out model-ab-results.json \
      --markdown-out model-ab-summary.md

  # operator lane: mint scoped keys automatically (master key present):
  LITELLM_URL=... LITELLM_MASTER_KEY=sk-master-... \
    python3 scripts/model-ab-compare.py --mint --max-budget 1.00 \
      --corpus scripts/model-ab-corpus.json --out model-ab-results.json

  # validate config + corpus with no network (CI / pre-flight):
  python3 scripts/model-ab-compare.py --dry-run --corpus scripts/model-ab-corpus.json

Deliberately dependency-free (stdlib only) so it runs in any agent pod or a bare
ubuntu image without a pip install.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

DEFAULT_MODELS = ["claude-haiku-4-5", "claude-opus-5"]

# Hard ceiling on live calls per model, independent of corpus size — a runaway
# corpus or --repeat can never issue more than this many calls to a model.
MAX_CALLS_PER_MODEL = 200

# LiteLLM stamps the per-call cost (its own price map applied) on this response
# header. Matched case-insensitively — proxies/versions vary the casing.
COST_HEADER = "x-litellm-response-cost"


class ConfigError(Exception):
    """Raised for a malformed corpus or an unresolvable run configuration."""


# ── Pure helpers (unit-tested, no I/O) ──────────────────────────────────────

def sanitize_model(model: str) -> str:
    """Model name → the SANITIZED token used in env var / flag names.

    claude-haiku-4-5 → CLAUDE_HAIKU_4_5. Non-alphanumerics collapse to '_' so a
    model id is a legal shell identifier; upper-cased for the env-var form.
    """
    return re.sub(r"[^0-9A-Za-z]+", "_", model).strip("_").upper()


def redact_key(key: str | None) -> str:
    """Never print a raw sk-... key. Show only a stable last-6 fingerprint."""
    if not key:
        return "<none>"
    if len(key) <= 6:
        return "sk-****"
    return f"sk-…{key[-6:]}"


def percentile(sorted_values: list[float], p: float) -> float:
    """Linear-interpolated percentile of an ALREADY-SORTED list. p in [0,100]."""
    if not sorted_values:
        return 0.0
    if len(sorted_values) == 1:
        return float(sorted_values[0])
    rank = (p / 100.0) * (len(sorted_values) - 1)
    lo = int(rank)
    hi = min(lo + 1, len(sorted_values) - 1)
    frac = rank - lo
    return float(sorted_values[lo] + (sorted_values[hi] - sorted_values[lo]) * frac)


def summarize_latencies(latencies_ms: list[float]) -> dict:
    """Distribution summary over per-turn wall-clock latencies (ms)."""
    vals = sorted(float(x) for x in latencies_ms)
    n = len(vals)
    if n == 0:
        return {"n": 0, "mean_ms": 0.0, "p50_ms": 0.0, "p90_ms": 0.0,
                "max_ms": 0.0, "min_ms": 0.0}
    return {
        "n": n,
        "mean_ms": round(sum(vals) / n, 1),
        "p50_ms": round(percentile(vals, 50), 1),
        "p90_ms": round(percentile(vals, 90), 1),
        "max_ms": round(vals[-1], 1),
        "min_ms": round(vals[0], 1),
    }


@dataclass
class TurnRecord:
    """One /v1/messages call: what it cost, how long it took, whether it worked."""
    task_id: str
    category: str
    ok: bool
    latency_ms: float
    cost_usd: float | None = None
    input_tokens: int | None = None
    output_tokens: int | None = None
    stop_reason: str | None = None
    error: str | None = None


def aggregate_model_result(model: str, records: list[TurnRecord]) -> dict:
    """Collapse a model's per-turn records into a cost + latency summary.

    cost is summed over successful turns only (a failed call has no cost).
    Latency stats are over successful turns (a timeout/error is not a latency
    sample). error/success counts are reported so a partial run is never
    silently read as a clean one.
    """
    ok = [r for r in records if r.ok]
    costs = [r.cost_usd for r in ok if r.cost_usd is not None]
    have_all_costs = len(costs) == len(ok) and len(ok) > 0
    latency = summarize_latencies([r.latency_ms for r in ok])
    per_category: dict[str, dict] = {}
    for r in ok:
        c = per_category.setdefault(
            r.category, {"turns": 0, "cost_usd": 0.0, "latency_ms": []})
        c["turns"] += 1
        if r.cost_usd is not None:
            c["cost_usd"] = round(c["cost_usd"] + r.cost_usd, 6)
        c["latency_ms"].append(r.latency_ms)
    for c in per_category.values():
        c["latency"] = summarize_latencies(c.pop("latency_ms"))
    total_cost = round(sum(costs), 6) if costs else 0.0
    return {
        "model": model,
        "turns_total": len(records),
        "turns_ok": len(ok),
        "turns_error": len(records) - len(ok),
        "cost_usd_total": total_cost,
        "cost_complete": have_all_costs,
        "cost_usd_per_turn": round(total_cost / len(ok), 6) if ok else 0.0,
        "input_tokens_total": sum(r.input_tokens or 0 for r in ok),
        "output_tokens_total": sum(r.output_tokens or 0 for r in ok),
        "latency": latency,
        "per_category": per_category,
        "errors": [
            {"task_id": r.task_id, "error": r.error}
            for r in records if not r.ok
        ],
    }


def build_tradeoff(summaries: list[dict], baseline: str, comparand: str) -> dict:
    """Cost + latency ratios of `comparand` relative to `baseline`.

    e.g. baseline=claude-haiku-4-5, comparand=claude-opus-5 →
    cost_ratio 30.0 means Opus-5 cost 30x Haiku on this corpus. Ratios are None
    when the baseline term is zero (undefined) so a divide never fabricates a
    number.
    """
    by_model = {s["model"]: s for s in summaries}
    b, c = by_model.get(baseline), by_model.get(comparand)
    if not b or not c:
        return {"baseline": baseline, "comparand": comparand,
                "note": "one or both models missing from results"}

    def ratio(num: float, den: float) -> float | None:
        return round(num / den, 3) if den else None

    return {
        "baseline": baseline,
        "comparand": comparand,
        "cost_ratio": ratio(c["cost_usd_total"], b["cost_usd_total"]),
        "cost_per_turn_ratio": ratio(c["cost_usd_per_turn"], b["cost_usd_per_turn"]),
        "latency_p50_ratio": ratio(c["latency"]["p50_ms"], b["latency"]["p50_ms"]),
        "latency_mean_ratio": ratio(c["latency"]["mean_ms"], b["latency"]["mean_ms"]),
        "cost_complete": bool(b.get("cost_complete") and c.get("cost_complete")),
    }


def render_markdown(report: dict) -> str:
    """Human-readable summary for a bundle / MR comment. Never leaks a key."""
    lines: list[str] = []
    lines.append("# Haiku vs Opus-5 — cost / latency A-B")
    meta = report.get("meta", {})
    lines.append("")
    lines.append(f"- corpus: `{meta.get('corpus')}` "
                 f"({meta.get('corpus_tasks')} tasks x {meta.get('repeat')} repeat)")
    lines.append(f"- proxy: `{meta.get('litellm_url')}`")
    lines.append(f"- captured: {meta.get('captured_at')}")
    lines.append("")
    lines.append("| model | turns ok/total | cost $ total | $/turn | "
                 "p50 ms | p90 ms | mean ms |")
    lines.append("|---|---|---|---|---|---|---|")
    for s in report.get("models", []):
        lat = s["latency"]
        cost_note = "" if s.get("cost_complete") else " ⚠"
        lines.append(
            f"| `{s['model']}` | {s['turns_ok']}/{s['turns_total']} | "
            f"{s['cost_usd_total']:.6f}{cost_note} | {s['cost_usd_per_turn']:.6f} | "
            f"{lat['p50_ms']:.0f} | {lat['p90_ms']:.0f} | {lat['mean_ms']:.0f} |")
    t = report.get("tradeoff")
    if t and t.get("cost_ratio") is not None:
        lines.append("")
        lines.append(
            f"**Tradeoff** ({t['comparand']} vs {t['baseline']}): "
            f"cost x{t['cost_ratio']}, "
            f"latency(p50) x{t['latency_p50_ratio']}."
        )
        if not t.get("cost_complete"):
            lines.append("")
            lines.append("> ⚠ Some turns returned no cost header — totals are a "
                         "lower bound. See `errors` / `cost_complete` in the JSON.")
    return "\n".join(lines) + "\n"


# ── Corpus loading + validation ─────────────────────────────────────────────

@dataclass
class Task:
    id: str
    category: str
    user: str
    max_tokens: int
    system: str | None = None
    description: str = ""


def load_corpus(path: str) -> list[Task]:
    """Load + strictly validate the task corpus. Raises ConfigError on any
    malformed entry — a silent skip would shrink the corpus and quietly change
    what the A-B measures."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)
    except FileNotFoundError as e:
        raise ConfigError(f"corpus not found: {path}") from e
    except json.JSONDecodeError as e:
        raise ConfigError(f"corpus is not valid JSON: {e}") from e

    tasks_raw = doc.get("tasks") if isinstance(doc, dict) else None
    if not isinstance(tasks_raw, list) or not tasks_raw:
        raise ConfigError("corpus must have a non-empty 'tasks' array")

    tasks: list[Task] = []
    seen: set[str] = set()
    for i, raw in enumerate(tasks_raw):
        if not isinstance(raw, dict):
            raise ConfigError(f"task[{i}] is not an object")
        tid = raw.get("id")
        if not isinstance(tid, str) or not tid:
            raise ConfigError(f"task[{i}] missing non-empty string 'id'")
        if tid in seen:
            raise ConfigError(f"duplicate task id: {tid!r}")
        seen.add(tid)
        for k in ("category", "user"):
            if not isinstance(raw.get(k), str) or not raw.get(k):
                raise ConfigError(f"task {tid!r} missing non-empty string {k!r}")
        mt = raw.get("max_tokens")
        if not isinstance(mt, int) or isinstance(mt, bool) or mt <= 0:
            raise ConfigError(f"task {tid!r} 'max_tokens' must be a positive int")
        system = raw.get("system")
        if system is not None and not isinstance(system, str):
            raise ConfigError(f"task {tid!r} 'system' must be a string when present")
        tasks.append(Task(
            id=tid, category=raw["category"], user=raw["user"], max_tokens=mt,
            system=system, description=str(raw.get("description", "")),
        ))
    return tasks


# ── Key resolution ──────────────────────────────────────────────────────────

def resolve_keys(models: list[str], cli_keys: dict[str, str],
                 env: dict[str, str]) -> dict[str, str]:
    """Map each model → its scoped virtual key, from --key-<model> then
    env LITELLM_KEY_<SANITIZED>. Returns only the resolved ones; the caller
    decides whether missing keys are fatal (they are not when --mint is set)."""
    resolved: dict[str, str] = {}
    for m in models:
        if m in cli_keys and cli_keys[m]:
            resolved[m] = cli_keys[m]
            continue
        env_name = f"LITELLM_KEY_{sanitize_model(m)}"
        if env.get(env_name):
            resolved[m] = env[env_name]
    return resolved


def plan_run(models: list[str], tasks: list[Task], repeat: int,
             keys: dict[str, str], will_mint: bool, max_budget: float,
             litellm_url: str | None) -> dict:
    """The fully-resolved run plan — the object --dry-run prints. Pure: no
    network, no key material (keys are shown redacted)."""
    calls_per_model = len(tasks) * repeat
    return {
        "models": models,
        "corpus_tasks": len(tasks),
        "repeat": repeat,
        "calls_per_model": calls_per_model,
        "calls_total": calls_per_model * len(models),
        "max_calls_per_model": MAX_CALLS_PER_MODEL,
        "litellm_url": litellm_url or "<unset>",
        "keys": {m: (redact_key(keys.get(m)) if not will_mint else "<mint-at-run>")
                 for m in models},
        "mint": will_mint,
        "max_budget_usd": max_budget if will_mint else None,
    }


# ── Live I/O (network) ──────────────────────────────────────────────────────

def _http_json(url: str, method: str, headers: dict, body: dict | None,
               timeout: float) -> tuple[int, dict, dict]:
    """POST/GET JSON. Returns (status, response_headers_lower, parsed_body).
    Non-2xx does NOT raise — the caller inspects status so a 429/4xx is data."""
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
            hdrs = {k.lower(): v for k, v in resp.headers.items()}
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        hdrs = {k.lower(): v for k, v in (e.headers or {}).items()}
        status = e.code
    try:
        parsed = json.loads(raw) if raw else {}
    except json.JSONDecodeError:
        parsed = {"_raw": raw[:500]}
    return status, hdrs, parsed


def mint_key(litellm_url: str, master_key: str, model: str, alias: str,
             max_budget: float, timeout: float) -> str:
    """Mint a budget-capped, model-scoped virtual key (POST /key/generate)."""
    status, _, body = _http_json(
        f"{litellm_url}/key/generate", "POST",
        {"Authorization": f"Bearer {master_key}", "Content-Type": "application/json"},
        {"key_alias": alias, "max_budget": max_budget, "models": [model],
         "metadata": {"purpose": "model-ab-compare", "model": model}},
        timeout)
    key = body.get("key") if isinstance(body, dict) else None
    if status // 100 != 2 or not isinstance(key, str) or not key.startswith("sk-"):
        raise ConfigError(f"mint failed for {model} (HTTP {status}): "
                          f"{json.dumps(body)[:200]}")
    return key


def delete_key(litellm_url: str, master_key: str, key: str, timeout: float) -> None:
    """Revoke a minted key (POST /key/delete). Best-effort; logs on failure."""
    try:
        status, _, body = _http_json(
            f"{litellm_url}/key/delete", "POST",
            {"Authorization": f"Bearer {master_key}",
             "Content-Type": "application/json"},
            {"keys": [key]}, timeout)
        if status // 100 != 2:
            print(f"WARN: key delete failed ({redact_key(key)}) HTTP {status}: "
                  f"{json.dumps(body)[:200]}", file=sys.stderr)
    # Broad catch is intentional: teardown revokes a minted key and must never
    # mask the run's own outcome — any delete failure is logged, not raised.
    except Exception as e:  # noqa: BLE001
        print(f"WARN: key delete raised for {redact_key(key)}: {e}",
              file=sys.stderr)


def key_spend(litellm_url: str, master_key: str, key: str,
              timeout: float) -> float | None:
    """LiteLLM's authoritative cumulative spend for a key (GET /key/info) — the
    independent cross-check against the summed response-cost headers."""
    status, _, body = _http_json(
        f"{litellm_url}/key/info?key={key}", "GET",
        {"Authorization": f"Bearer {master_key}"}, None, timeout)
    if status // 100 != 2 or not isinstance(body, dict):
        return None
    info = body.get("info", body)
    spend = info.get("spend") if isinstance(info, dict) else None
    return float(spend) if isinstance(spend, (int, float)) else None


def run_turn(litellm_url: str, key: str, model: str, task: Task,
             timeout: float) -> TurnRecord:
    """One /v1/messages call. Measures wall-clock latency; reads cost from the
    response header and tokens from the usage block. Any failure is captured on
    the record — one bad turn never aborts the run."""
    messages = [{"role": "user", "content": task.user}]
    body = {"model": model, "max_tokens": task.max_tokens, "messages": messages}
    if task.system:
        body["system"] = task.system
    headers = {
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
        "anthropic-version": "2023-06-01",
    }
    start = time.monotonic()
    try:
        status, resp_headers, parsed = _http_json(
            f"{litellm_url}/v1/messages", "POST", headers, body, timeout)
    # Broad catch is intentional: a network/transport error is a recorded per-turn
    # outcome (ok=False), so one bad turn never aborts the whole A-B run.
    except Exception as e:  # noqa: BLE001
        return TurnRecord(task.id, task.category, ok=False,
                          latency_ms=round((time.monotonic() - start) * 1000, 1),
                          error=f"request raised: {e}")
    latency_ms = round((time.monotonic() - start) * 1000, 1)
    if status // 100 != 2:
        snippet = json.dumps(parsed)[:200]
        return TurnRecord(task.id, task.category, ok=False, latency_ms=latency_ms,
                          error=f"HTTP {status}: {snippet}")
    cost = resp_headers.get(COST_HEADER)
    try:
        cost_usd = float(cost) if cost is not None else None
    except ValueError:
        cost_usd = None
    usage = parsed.get("usage") if isinstance(parsed, dict) else None
    in_tok = out_tok = None
    if isinstance(usage, dict):
        in_tok = usage.get("input_tokens")
        out_tok = usage.get("output_tokens")
    return TurnRecord(
        task.id, task.category, ok=True, latency_ms=latency_ms, cost_usd=cost_usd,
        input_tokens=in_tok, output_tokens=out_tok,
        stop_reason=parsed.get("stop_reason") if isinstance(parsed, dict) else None,
    )


def utcnow_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def run(models: list[str], tasks: list[Task], repeat: int, litellm_url: str,
        keys: dict[str, str], timeout: float,
        verify_key_spend_master: str | None = None) -> dict:
    """Drive the corpus through each model and assemble the report dict."""
    calls_per_model = len(tasks) * repeat
    if calls_per_model > MAX_CALLS_PER_MODEL:
        raise ConfigError(
            f"planned {calls_per_model} calls/model exceeds cap "
            f"{MAX_CALLS_PER_MODEL} (corpus={len(tasks)} x repeat={repeat})")

    summaries: list[dict] = []
    key_spend_xcheck: dict[str, dict] = {}
    for model in models:
        key = keys[model]
        records: list[TurnRecord] = []
        for _ in range(repeat):
            for task in tasks:
                rec = run_turn(litellm_url, key, model, task, timeout)
                records.append(rec)
                status = "ok" if rec.ok else f"ERR {rec.error}"
                print(f"[{model}] {task.id}: {rec.latency_ms:.0f}ms "
                      f"cost={rec.cost_usd} {status}", file=sys.stderr)
        summary = aggregate_model_result(model, records)
        if verify_key_spend_master:
            settled = key_spend(litellm_url, verify_key_spend_master, key, timeout)
            if settled is not None:
                key_spend_xcheck[model] = {
                    "header_sum_usd": summary["cost_usd_total"],
                    "key_info_spend_usd": round(settled, 6),
                    "delta_usd": round(settled - summary["cost_usd_total"], 6),
                }
        summaries.append(summary)

    report = {
        "meta": {
            "captured_at": utcnow_iso(),
            "litellm_url": litellm_url,
            "corpus_tasks": len(tasks),
            "repeat": repeat,
            "models": models,
        },
        "models": summaries,
        "tradeoff": build_tradeoff(summaries, models[0], models[-1])
        if len(models) >= 2 else None,
    }
    if key_spend_xcheck:
        report["key_spend_xcheck"] = key_spend_xcheck
    return report


# ── CLI ─────────────────────────────────────────────────────────────────────

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--corpus", default="scripts/model-ab-corpus.json",
                   help="path to the task corpus JSON")
    p.add_argument("--models", default=",".join(DEFAULT_MODELS),
                   help="comma-separated model ids (baseline first)")
    p.add_argument("--repeat", type=int, default=1,
                   help="times to run each task per model (latency stability)")
    p.add_argument("--url", default=os.environ.get("LITELLM_URL"),
                   help="LiteLLM proxy base URL (env LITELLM_URL)")
    p.add_argument("--key", action="append", default=[], metavar="MODEL=sk-...",
                   help="scoped virtual key for a model (repeatable)")
    p.add_argument("--mint", action="store_true",
                   help="mint+revoke a scoped key per model (needs LITELLM_MASTER_KEY)")
    p.add_argument("--max-budget", type=float, default=1.00,
                   help="per-key budget cap (USD) when --mint (default 1.00)")
    p.add_argument("--verify-key-spend", action="store_true",
                   help="cross-check header cost vs /key/info (needs LITELLM_MASTER_KEY)")
    p.add_argument("--timeout", type=float, default=120.0,
                   help="per-request timeout seconds")
    p.add_argument("--out", help="write the full results JSON here")
    p.add_argument("--markdown-out", help="write a human-readable summary here")
    p.add_argument("--dry-run", action="store_true",
                   help="validate corpus + config and print the plan; no network")
    return p.parse_args(argv)


def _parse_cli_keys(pairs: list[str]) -> dict[str, str]:
    out: dict[str, str] = {}
    for pair in pairs:
        if "=" not in pair:
            raise ConfigError(f"--key must be MODEL=sk-..., got {pair!r}")
        model, _, key = pair.partition("=")
        out[model.strip()] = key.strip()
    return out


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    try:
        tasks = load_corpus(args.corpus)
        models = [m.strip() for m in args.models.split(",") if m.strip()]
        if not models:
            raise ConfigError("--models resolved to an empty list")
        cli_keys = _parse_cli_keys(args.key)
        keys = resolve_keys(models, cli_keys, dict(os.environ))
        master = os.environ.get("LITELLM_MASTER_KEY")

        if args.dry_run:
            plan = plan_run(models, tasks, args.repeat, keys, args.mint,
                            args.max_budget, args.url)
            print(json.dumps({"dry_run": True, "plan": plan}, indent=2))
            # Validate the resolvable-ness of the run so --dry-run is a real gate.
            if not args.mint:
                missing = [m for m in models if m not in keys]
                if missing:
                    print(f"NOTE: no key resolved for {missing} — supply "
                          f"--key/env or use --mint before a live run.",
                          file=sys.stderr)
            return 0

        if not args.url:
            raise ConfigError("LITELLM_URL (or --url) is required for a live run")
        litellm_url = args.url.rstrip("/")

        minted: list[str] = []
        try:
            if args.mint:
                if not master:
                    raise ConfigError("--mint requires LITELLM_MASTER_KEY in env")
                stamp = utcnow_iso().replace(":", "").replace("-", "")
                for m in models:
                    alias = f"model-ab-{sanitize_model(m).lower()}-{stamp}"
                    k = mint_key(litellm_url, master, m, alias,
                                 args.max_budget, args.timeout)
                    keys[m] = k
                    minted.append(k)
                    print(f"minted scoped key for {m}: {redact_key(k)} "
                          f"(budget ${args.max_budget})", file=sys.stderr)
            missing = [m for m in models if not keys.get(m)]
            if missing:
                raise ConfigError(
                    f"no scoped key for {missing}; supply --key MODEL=sk-... / "
                    f"env LITELLM_KEY_<M>, or pass --mint with LITELLM_MASTER_KEY")

            verify_master = master if args.verify_key_spend else None
            report = run(models, tasks, args.repeat, litellm_url, keys,
                         args.timeout, verify_key_spend_master=verify_master)
        finally:
            for k in minted:
                delete_key(litellm_url, master, k, args.timeout)

        out_json = json.dumps(report, indent=2)
        if args.out:
            with open(args.out, "w", encoding="utf-8") as fh:
                fh.write(out_json + "\n")
            print(f"wrote {args.out}", file=sys.stderr)
        else:
            print(out_json)
        if args.markdown_out:
            with open(args.markdown_out, "w", encoding="utf-8") as fh:
                fh.write(render_markdown(report))
            print(f"wrote {args.markdown_out}", file=sys.stderr)

        # Exit non-zero if any model had ZERO successful turns — a run that
        # measured nothing must not read as green.
        if any(s["turns_ok"] == 0 for s in report["models"]):
            print("ERROR: at least one model produced no successful turns",
                  file=sys.stderr)
            return 1
        return 0
    except ConfigError as e:
        print(f"config error: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
