# LiteLLM Virtual-Key ACL Widening (and the retired 30-day rotation)

**Bead:** ka-0000002201 · **Decision:** D-h2 (ka-0000006032, resolved `30d`) ·
**Epic:** ka-0000005228 (in-cluster gardenio-litellm hardening) ·
**Rotation deleted:** gd-OLYrS5E6F8 (audit gd-p9wTp4Yfer)

Each credential **account bead** carries a LiteLLM **virtual key**
(`litellm_key`) that the controller distributes to agent pods and uses for
spend tracking. The controller runs one loop over those keys: it widens each
key's model allow-list so a newly-served model group reaches keys minted
before it existed.

## Retired: POC 30-day key rotation

Decision **D-h2** set a POC rotation cadence of 30 days for these keys, and the
loop shipped with a rotation half gated by `LITELLM_KEY_ROTATION_ENABLED`. That
gate had no chart knob and was never set in any environment, so the loop always
ran widen-only. The rotation half was deleted in gd-OLYrS5E6F8, along with
`LITELLM_KEY_ROTATION_ENABLED` and `LITELLM_KEY_ROTATION_MAX_AGE`.

Virtual keys are **not rotated** by the controller. Any
`litellm_key_rotated_at` / `litellm_key_rotation_count` fields on an account
bead are leftovers and nothing reads them. They are distinct from the
account-failover fields (`last_rotated_at` / `rotation_count`, written by
`auth_failover.go`). Bringing rotation back is a fresh decision, not a flag
flip.

## Implementation

Controller-side loop:
[`controller/cmd/controller/key_acl_widen.go`](../controller/cmd/controller/key_acl_widen.go)
(`keyACLWidener`), started from `main.go` inside the LiteLLM block. It reconciles
once at startup, then every interval. On each pass it:

1. Lists `type=account` beads (`open` / `in_progress`) that carry a
   non-empty `litellm_key`.
2. Fetches the served model groups once (`GET /v1/models`). If the fetch fails
   or returns nothing, the pass is skipped.
3. For each key, reads its model allow-list (`GET /key/info`) and adds every
   served Anthropic chat group (ids ending `-oauth` or starting `anthropic/`)
   via `POST /key/update`. It only ever adds. An empty list (= unrestricted) is
   never touched, and nothing is written when nothing changed.

The loop talks only to LiteLLM. It never writes an account bead.

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `LITELLM_KEY_ROTATION_INTERVAL` | `6h` | How often the widen loop scans account beads. The name predates the rotation deletion. The loop requires `LITELLM_URL` + `LITELLM_MASTER_KEY`. |

## Verifying a widen

Controller logs (`component=key-acl`) print `key acl widen loop started`, and
`key acl: widened` with the account and the added model groups each time a key
gains a group. `key acl: widen failed` and `key acl: served-model fetch failed`
are logged at warn.
