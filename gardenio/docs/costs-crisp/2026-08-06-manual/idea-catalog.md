# /costs CRISP — honest-cut idea catalog (iteration 2026-08-06-manual)

Short list of missing HONEST cost cuts an operator cannot currently read on /costs.
Each is grounded in a real source. No metered-API estimation — the read-only cost
contract holds (LiteLLM stays authoritative, epic ka-0000000758).

| Title | Surface | Source | Operator question it answers |
|---|---|---|---|
| Per-account 5h-vs-7d window headroom | per-account cards | `spend_5h_window_usd` / `spend_7d_window_usd` + `throttle_reset_*_at` account beads | "How close is each subscription account to its rolling quota right now, and when does it reset?" |
| Subscription-vs-metered fleet split | summary card | account-bead `billing_type` | "What fraction of the fleet's spend is subscription (quota-bound) vs metered (dollar-bound)?" |
| Cost-per-closed-bead efficiency | new tile | `gardenio_agent_spend_logs_usd` ÷ closed-bead count per project | "Which projects convert dollars into closed work most efficiently?" |
| Throttle-event timeline vs utilisation | new panel | `throttle_reset_*_at` + `utilization_*` account fields | "Do throttle events correlate with utilisation spikes, or are we hitting quota while idle?" |
| Estimate-vs-settled trend per project | trust strip extension | `gardenio_xcheck_agreement` over time | "Is the estimate/settled reconciliation drifting for any one project?" |

Carry forward as Presentation/Depth beads only if a future full render scores the
page below bar. This iteration did not render the live page (access gap), so none
are filed as work yet.
