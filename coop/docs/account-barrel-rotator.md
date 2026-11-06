# Account Barrel Rotator — Design

Epic: ka-0000000790 · Design task: ka-0000002767

Replace the least-loaded selector in `CredentialBroker::assign_account[_for_project]`
(`coop/crates/mux/src/credential/broker.rs:158`, `:197`) with a round-robin
"barrel" rotator so new sessions distribute evenly across eligible accounts.
Feature-flagged; least-loaded stays the default until the rotator is proven.

## Scope

The rotator governs **which account a new session uses at start time**, and
nothing else. Already-running sessions keep whatever account they were
assigned — the rotator never interrupts a live session to move it, and no
code path reassigns `assigned_account` after registration. "Fairness" here
means fairness over the stream of new assignments, not steady-state
redistribution of the live pool.

## Why not least-loaded

- **Snap-to-zero pile-up.** A draining account's count is lowest, so every
  new session in the next burst lands on it until it catches up.
- **Lifetime imbalance.** Point-in-time counts ignore session *duration*, so
  long-lived sessions quietly concentrate on a few accounts.
- **Nondeterministic tie-breaking.** Ties resolve by `HashMap` iteration order.
- **Preferred short-circuits fairness.** A healthy preferred account is
  returned without load balancing, so controller-pushed preferences defeat
  any pool-wide fairness effort.

## Decisions

### 1. Cursor placement — global

Single `AtomicUsize` cursor on `CredentialBroker`. Per-project cursors would
fairness-balance each project in isolation but split the rotator state and
make reasoning about global utilization harder; per-project calls walk the
same global cursor, filtering by the authorized set.

Rationale: fairness is a pool-level property. With one cursor, N projects
sharing accounts still produce globally even assignment over time. The small
"phase" difference this causes per-project is acceptable and the
`session_counts`/new lifetime-count metric already expose any residual skew.

### 2. Account ordering — lexicographic on name

Build the eligible list by sorting account names lexicographically. Stable
across restarts because account names are stable. When accounts are added
or removed, the cursor is taken `mod new_len`; this creates a short
unfairness window (at most one rotation cycle) which is acceptable given
that add/remove is rare compared to assignment rate.

Alternative considered: sort by a persistent `rank` field. Rejected — adds
schema + controller changes for negligible fairness benefit.

### 3. Eligibility predicate — Healthy only

A name is **eligible** iff its state is `AccountStatus::Healthy`. Warming,
Throttled, and Missing are skipped (same as today). For the per-project
path, eligibility is applied first, then intersected with the `authorized`
set — so unauthorized-but-healthy accounts are excluded and
authorized-but-warming accounts are skipped.

This preserves the current behavior that Warming = soft-steering away
from new sessions without changing state.

### 4. Preferred-account policy — honor if Healthy

If `preferred` is supplied and Healthy (and, for the per-project path,
authorized), return it without consulting the cursor. Otherwise, advance
the cursor and return the next eligible account. This keeps
project→account binding (ka-0000005133) working and matches current
behavior at start time.

No "bypass" mode is defined: because the rotator only runs at session
start (see Scope), there is no caller that needs to override `preferred`
to forcibly rebalance. Existing signatures are unchanged:

```rust
pub async fn assign_account(
    &self,
    preferred: Option<&str>,
) -> Option<String>;

pub async fn assign_account_for_project(
    &self,
    preferred: Option<&str>,
    authorized: &HashSet<String>,
) -> Option<String>;
```

### 5. Feature flag — `COOPMUX_ASSIGNMENT_STRATEGY`

Env var read once at broker construction, stored as an enum on the broker:

```rust
enum AssignmentStrategy { LeastLoaded, RoundRobin }
```

Values: `least_loaded` (default), `round_robin`. Unrecognised values fall
back to `least_loaded` with a `tracing::warn!`.

**Rollout plan:**

1. Land the implementation with flag defaulted to `least_loaded`; all
   existing tests continue to pass unchanged.
2. Add a rotator fairness test and a rotator-specific set of
   assign/preferred honor-if-Healthy tests (see §6).
3. Enable `round_robin` on `gardenio-rwx` (staging) via Helm values; monitor
   `/pool` for a 24–48 h window, watching max-min session-count spread and
   per-account lifetime assignment count.
4. If metrics are within expectations (max-min ≤ 1 under steady-state,
   lifetime counts within ±10 % over the window), flip prod to
   `round_robin`.
5. Rollback is a Helm values change — no redeploy of coopmux image needed
   because the env var is read at startup and the broker is recreated on
   pod restart.

### 6. Observability — `/pool` additions

Extend `PoolAccountInfo` with a lifetime counter, and extend the top-level
`/pool` response with the cursor state:

```rust
pub struct PoolAccountInfo {
    pub name: String,
    pub provider: String,
    pub status: AccountStatus,
    pub session_count: u32,           // existing — live count
    pub lifetime_assigned: u64,       // NEW — monotonic, reset on restart
    pub has_api_key: bool,
}

pub struct PoolStatus {
    pub accounts: Vec<PoolAccountInfo>,
    pub strategy: &'static str,       // "least_loaded" | "round_robin"
    pub cursor: Option<usize>,        // NEW — None if strategy = least_loaded
}
```

`lifetime_assigned` is bumped from `session_assigned` (alongside
`session_counts`) and is the key metric for fairness in the rollout window.

## Implementation sequence

Subsequent child tasks (already on the epic):

1. **Implement** — add `AssignmentStrategy`, rotator cursor, wire flag.
   Don't change defaults. Don't touch distributor.
2. **Tests** — fairness test (N=4 accounts, K=400 assignments under
   mixed lifetimes, assert max-min ≤ 1); preferred honor-if-Healthy
   test; churn test (add/remove account mid-run, assert cursor recovers
   within one cycle); authorized-set tests.
3. **Metrics** — `/pool` extensions + surface in `coopmux` metrics
   endpoint.
4. **Rollout** — Helm values change for staging, then prod; document
   revert steps in the epic.

## Out of scope

- **Moving or reassigning live sessions.** Once a session has registered
  with an `assigned_account`, nothing in the rotator changes it. No
  "drain this account" or "rebalance after failover" flow is introduced;
  those remain the distributor's concern and are explicitly out of scope
  for this epic.
- Weighted rotation, cross-instance fairness, touching the distributor
  failover/rebalance paths — noted in the epic.

## Risks

- **Starvation on small pools.** With N=2 accounts and an unlucky
  Warming→Healthy oscillation, rotator could briefly skew. Mitigated by
  lifetime-count visibility; acceptable because small pools already have
  little room for unfairness.
- **Cursor drift after churn.** Documented above; one-cycle unfairness
  window is acceptable given the metric visibility.
- **Test flake.** Fairness tests on randomized lifetimes can flake under
  CI scheduler variance. Use deterministic session-lifetime sequences
  (seeded RNG) and assert max-min bounds that hold regardless of
  scheduling, not raw equality.
