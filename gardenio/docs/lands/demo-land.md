# The demo land: try gardenio with no cluster

Task gd-c6LzZkk0qy. The owner chose a hosted demo over a docker compose file
(decision gd-cGMRpc4uLM, `hosted`). Someone with no Kubernetes cluster signs in
with GitHub and gets a **guest pass** to one land the operator set aside. With
the pass they run `gb ping` and `kd list` from their own terminal against the
project's daemon. No docker and no build are needed.

The demo is an ordinary tenant land. What is new is a self-serve door that hands
out guest passes, and an expiry on those passes. Everything else is Lands as it
already runs: the GitHub sign-in, the land door on `api.clonk.tv`, the guest
tier and the invite pass.

## What a stranger does

1. Open `https://<viewer>/lands/demo`. The Lands home page links it ("Just
   looking?") once the demo is configured.
2. Sign in with GitHub, then read and accept the terms. These are the same terms
   a land owner accepts. GitHub sign-in is otherwise invite-only, but the edge
   admits any signed-in account to `/lands/demo` and the terms page
   `/lands/accept` (owner decision gd-T37nCeWMhq = open; the check is
   `/oauth2/auth-lands-demo` in `public-edge.yaml`). Every other `/lands/` path
   still answers "You need an invite" until the pass is joined.
3. Select **Get a guest pass**. Within a minute, the next run of the provisioner
   adds them to the land's `/project-<land>/guests` group. The page then shows:

   ```sh
   export BEADS_HTTP_URL=https://api.clonk.tv/land/<land>
   gb login        # device code: open the link, sign in with GitHub again
   gb ping         # ok
   kd list         # the demo land's beads
   ```

   `gb login` against a `/land/<name>` URL requests the `land-<name>` scope and
   caches a session only for that land (`gardenio/clicreds/oidclogin`). Keycloak
   reads group membership when it mints a token. A session started **before**
   the pass was ready names no land, so the guest must sign in after it.

Through the land door, `gb ping` sends `GET /v1/me` rather than `GET /v1/health`.
The door lists no credential-free route (`kbeads/internal/server/land_door_routes.txt`),
so `/v1/health` is a 404 there. The door's own admission check is the probe
instead, and an `ok` from it also proves that the sign-in got in.

## What a guest can and cannot do

The guest tier is enforced by kbeads on the guest's own bearer
(`kbeads/internal/server/action_table.go`, Commons spec §6). A guest can:

- read the land: tickets, plans, decisions, channels, the roster;
- post messages and reactions in the land's channels.

A guest cannot spawn an agent, approve an escalation, answer a decision, close
work, or manage channels or members. The land door also keeps every token,
admin, hook, embedding, SSE and config route closed (`land_door_routes.txt`).
They never see another land, because the door narrows a land-audience token to
its one land.

## Abuse limits

| Limit | Value | Where |
|---|---|---|
| GitHub account required | The pass is filed for the verified `github_id` claim, never one the caller typed | viewer `lands_demo.go` |
| Terms first | The current version must be accepted | viewer `lands_demo.go` |
| One pass per GitHub account | Asking again returns the open pass, on any viewer replica | viewer `lands_demo.go`; kbeads `land_invite.go` holds one open invite per account in a land |
| Open passes at once | `LANDS_DEMO_MAX_GUESTS`, default 50; then "every guest pass is taken" | viewer `lands_demo.go`, sent as the invite's `open_invite_cap`, which kbeads `land_invite.go` holds across replicas |
| Unclaimed pass | Expires one hour after it was asked for | `land_invite` `expires_at`, provisioner |
| Pass length | `LANDS_DEMO_GUEST_TTL` after the join, default 24h, minimum 1h; then the person is taken out of the land, and out of `lands-tenants` if it was their last land | provisioner `invites.go` |
| Access token | 5 minutes; a revoked pass stops working at the next refresh | Keycloak realm |
| Request rate | 10 r/s per IP at the edge (`api_per_ip`); 20 writes/s and 100 reads/s per identity at the daemon | `public-edge.yaml`, kbeads `rate_limit.go` |
| `/lands/` front door | 5 r/s per IP | `public-edge.yaml` `front_door_per_ip` |
| Edge | Any signed-in account reaches `/lands/demo` and `/lands/accept` only; the rest of `/lands/` stays invite-only | `public-edge.yaml` `/oauth2/auth-lands-demo`, `scripts/check-viewer-edge-groups.py` |
| Spend | Guests cannot spawn. The land's own agents (its steward) spend from the land's purse, and stop when it is empty | Lands purse, spawn wall |

A pass never removes someone who holds a higher role in the demo land. If the
operator's own GitHub account takes a pass, the pass ends and they keep their
role.

**The one cost a guest can cause** is the steward. A person's message in
`#general` wakes it, and its run spends credits. Give the demo land a small
credit grant and watch `gb lands purse <land>`. To make the demo read-only, run
`gb lands freeze <land>`: a frozen land refuses Commons posts and spawns, and
guests can still sign in, `gb ping` and `kd list`.

## Setting it up (operator)

Crews do not edit `gardenio/helm/**` or `deploy-values/**`, so the operator does
steps 1, 3 and 4 (escalation filed from gd-c6LzZkk0qy).

1. **GitHub sign-in.** Roll out the Keycloak GitHub IdP (Track B step 7.3,
   escalation gd-Ju9z2X57Qw). Without it a stranger has no way to make an
   account: local registration is off.
2. **Open the demo land.** As the operator, apply for a plot at `/lands/apply`
   with a public sample repository and the land name `demo` (any free DNS-label
   name works). Approve it with a small credit grant. The provisioner opens it
   as it opens every land: project bead, purse, Keycloak groups, channels,
   namespace and steward. Check with `gb lands provision demo`.
3. **Wire the settings.** The chart has no value for these yet, so it needs a
   small chart change first, then the values:

   | Env | Component | Value |
   |---|---|---|
   | `LANDS_DEMO_LAND` | beads-viewer **and** controller | `demo` |
   | `LANDS_DEMO_DAEMON_URL` | beads-viewer | `https://api.clonk.tv` (the page appends `/land/<land>`) |
   | `LANDS_DEMO_MAX_GUESTS` | beads-viewer | optional, default `50` |
   | `LANDS_DEMO_GUEST_TTL` | beads-viewer **and** controller, the same value | optional, default `24h` |

   The viewer needs its machine principal (`gardenio-beads-viewer`, already
   granted `gardenio-machine-lands-gate`, which writes `land_invite`). The
   controller needs the Lands provisioner on (`LANDS_PROVISION_ENABLED`), which
   runs the invite pass. A bad value keeps that component's demo off, and the
   component logs why: the viewer says `lands demo misconfigured`, and the
   provisioner refuses to start.
4. **Roll out** the viewer and controller in the next release window.
5. **Check it as a stranger.** Use a GitHub account that is not in any gardenio
   group. Walk through [What a stranger does](#what-a-stranger-does) and confirm
   that `gb ping` prints `ok` and `kd list` lists the demo land's beads. Then
   confirm that `gb lands purse demo` has not moved.

## Running it

```sh
kd list --type land_invite -l project:demo                # every pass: pending, joined, ended
kd list --type land_invite -l project:demo --status open  # the passes that count toward the cap
gb lands purse demo                                       # what the steward has spent
```

- **End one pass now.** In the Commons, open the demo land's People lightbox
  and revoke it. The next provisioner run takes the person out.
- **Make it read-only.** `gb lands freeze demo`. Undo it with `gb lands thaw demo`.
- **Wipe it.** Close the land (`gb lands close demo --reason "demo reset"`) and
  open a fresh one under the same name (step 2 above). Closing takes every guest
  out.
- **Turn it off.** Unset `LANDS_DEMO_LAND` on the **viewer** first: the door
  closes and no new passes are filed. Leave it set on the controller until the
  open passes have ended (at most `LANDS_DEMO_GUEST_TTL`), or revoke them. A
  controller without `LANDS_DEMO_LAND` treats the remaining passes as ordinary
  guest invites, and they never expire.

## Code

| Piece | File |
|---|---|
| The door: `GET`/`POST /lands/demo`, `routeApplicant` | `gardenio/controller/cmd/beads-viewer/lands_demo.go`, page `lands_demo` in `templates/lands.html` |
| Pass expiry | `gardenio/controller/internal/lands/provision/invites.go` (`demoPassOver`, `endDemoPass`) |
| Controller env | `gardenio/controller/cmd/controller/lands_provisioner.go` |
| `gb ping` through a land door | `gardenio/controller/cmd/gb/ping.go` |
| The invite type it reuses | `kbeads/internal/server/land_invite.go` (unchanged) |

Related: [provisioner.md](provisioner.md) (the invite pass, closing and freezing
a land), [keycloak-provisioner.md](keycloak-provisioner.md) (groups),
[../gb-login-runbook.md](../gb-login-runbook.md) (device code sign-in),
[support-runbook.md](support-runbook.md).
