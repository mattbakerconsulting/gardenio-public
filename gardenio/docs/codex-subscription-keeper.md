# Codex subscription refresh keeper

Runbook for the controller loop in `gardenio/controller/cmd/controller/codex_keeper.go`
(gd-6ggiM3hiV5). Design: ADR 0004 §5 and the credential contract on gd-3gTWiNEhg1.

## What it does

For each open account bead with `provider=openai` and `credential=secret:<name>#auth.json`
(default `gardenio-codex-auth`), every 5 minutes, only on the leader controller:

1. Reads the real login from Secret `<name>` (key `auth.json`).
2. If the access-token JWT `exp` is under 24h away, or `last_refresh` is older than 6 days,
   refreshes at `auth.openai.com/oauth/token`. The refresh token is single-use.
3. Writes in this order: Secret `<name>` (persist first) -> Secret `<name>-session`
   (placeholder `refresh_token`; the only one pods mount) -> coopmux
   `POST /api/v1/credentials/set` (`provider=openai`, token = the session document).
4. Reads `chatgpt.com/backend-api/wham/usage` and writes `utilization_5h|7d`,
   `throttle_reset_5h_at|7d_at|_at`, `representative_claim`, `throttle_status`,
   `probe_checked_at` onto the bead. `limit_reached` -> `throttle_status=rejected` and
   `status=exhausted` (same rule as the Anthropic probe). A failed or reshaped read changes
   nothing: the account stays eligible.

The keeper never rotates unless a `SelfSubjectAccessReview` says it may `update` both
Secrets, because a rotated token it cannot persist is a dead login. With the RBAC absent it
logs a warning per pass and does nothing.

## Failure handling

| Event | Keeper does |
|---|---|
| Refresh rejected (4xx other than 408/429: reused, expired, revoked) | Never retried. Sets `probe_auth_status=rejected`, `codex_rejected_refresh_fp=<fingerprint>`, `codex_auth_error=<code>`; files one re-login escalation (idempotent per fingerprint). |
| Refresh network error / 5xx / 408 / 429 | Warns; retries next pass. Does not condemn the account. |
| Rotated, but the write to `<name>` fails | Holds the rotated document in memory, retries each pass before anything else, files a P0 escalation. A controller restart before it lands loses the login. |
| `<name>` changed during the rotation (operator re-login) | Drops its rotated document; never overwrites the operator's login. |
| `<name>-session` or coopmux write fails | Self-heals next pass: the session document is derived from `<name>`. |

`probe_auth_status=rejected` is what `isHealthyAuthorized` already excludes. The rejected
token's fingerprint (first 8 bytes of its SHA-256) is stored so a later pass can tell a
re-login (different token in the Secret) from the same dead one, and clears the rejection by
itself.

## Re-login

Preferred (ADR 0007): press **Start device login** on the keeper's re-login escalation in
Slack. You need the `approve:escalation` capability. The leader controller asks OpenAI for a
device code and shows the URL and one-time code to you alone (ephemeral reply). Open the URL
in a browser signed in as the ChatGPT plan owner, enter the code (15 minute window), approve.
The controller writes the new login into `<name>` itself, comments the refresh fingerprint on
the escalation and closes it; the next keeper pass clears `probe_auth_status`. Approve only a
code you just requested. The broker refuses to replace a login the keeper has not found
rejected, a login for a different ChatGPT account (unless the request sets
`allow_account_change`), and a Secret that does not exist.

The routes behind the button (`POST /api/v1/codex/device-login`, `GET .../status`) are served
by the leader only and are off entirely when `CONTROLLER_API_TOKEN` is unset.

Fallback (Slack down, or the broker unavailable): `gb codex login --secret <name> -n <ns>` runs
`codex login --device-auth` against a throwaway `CODEX_HOME`, stores the file in `<name>` with
kubectl without displaying it, and removes the throwaway home. Never copy a laptop
`~/.codex/auth.json`.

First use: run `gb codex device-probe` once to confirm the protocol against auth.openai.com
(result discarded, only non-secret facts printed).

## Prerequisites

- RBAC: name-scoped `update` on `<name>` and `<name>-session`, bound so that only the
  controller holds it. The keeper does not need `create`: the operator pre-creates an empty
  `<name>-session`.
  The chart renders this from `agents.codexAuth.secretNames` (default `[gardenio-codex-auth]`):
  Role `<fullname>-agents-codex-keeper`, bound to the controller SA only. beads-viewer
  runs as `<fullname>-aux-sa` (same IRSA annotations, bound to no
  Role; gd-P84rleirJ4), so the controller is the sole holder.
  Verify after the roll: `kubectl auth can-i update secret/gardenio-codex-auth --as
  system:serviceaccount:<ns>:<fullname>-aux-sa` -> no; `--as ...:<fullname>-agents-sa` -> yes.
- Leader election is always on (`ENABLE_LEADER_ELECTION=true`) and the controller's
  `terminationGracePeriodSeconds` is at least 90 (`agents.terminationGracePeriodSeconds`), above
  the keeper's 65s shutdown drain budget.
- Overrides for hermetic tests: `CODEX_REFRESH_TOKEN_URL_OVERRIDE`, `CODEX_USAGE_URL_OVERRIDE`.
