# The Lands provisioner

Step 8.6 (gd-G9ZfREbe8z) of the [Track B spec](../superpowers/specs/2026-10-19-lands-track-b-design.md),
§3.3. A controller loop (`gardenio/controller/internal/lands/provision/`, wired in
`cmd/controller/lands_provisioner.go`) that turns an approved `plot_application` into a land, and
takes a land apart when the operator closes it. It runs once a minute, and is off unless
`LANDS_PROVISION_ENABLED=true`.

## Provisioning

An application in state `approved` moves to `provisioning` (`provision_attempt` counts the
attempts) and runs these steps in order. Each step is idempotent, and records itself in
`provision_step` when it is done; a pass resumes after the last recorded step, so a controller that
dies between any two steps finishes the land on its next pass.

| Step | What it does |
|---|---|
| `project` | Creates the `type:project` bead `<land>` (label `project:<land>`, idempotency key per application): `git_url` and `repos` for `github.com/<repo>`, `default_branch` read from GitHub through the applicant's Lands App installation (a `metadata: read` token, revoked after use), `prefix` from the name, `land_kind=tenant`, `land_owner_sub`, `land_application_id`, `land_slack_mirror=off`, `github_app=lands`, `github_installation_id`, `max_live_agents=3`, triggers and schedules off, the approval's `model`. A live project of that name made for another application stops the step. |
| `purse` | Needs a credit rate in force (`gb lands rates set`), or fails closed. Creates the account bead `lands-<land>` from `LANDS_ACCOUNT_TEMPLATE` (its provider, base URL, credential and model; never its key), writes a new LiteLLM key value onto the account **before** LiteLLM learns it, creates the key in LiteLLM under that value (alias `lands-<land>`, born at `max_budget` 0), writes the approval's `credit_grant` (idempotency key per application), sets the key's `max_budget` from the whole ledger (credits ÷ `llm_multiplier`, rounded down to the cent) and pins the project's `account`. Because the key value is chosen first, a crash never leaves an orphaned key. |
| `keycloak` | `EnsureProjectGroup`, `EnsureLandRoleGroup(owner)`, the applicant (their Keycloak subject is their user id) into `/project-<land>/owners`, and into `lands-tenants`. `ErrReservedLand`, `ErrNotLandGroup` and `ErrForeignRoles` fail the step; nothing is repaired. |
| `channels` | `#general`, `#crew`, `#decisions`, `#escalations`, `#support` (channel_kind `user`) and `#machines` (channel_kind `machines`, where the land's agents and the controller talk; steward amendment 2026-10-23 §2), all `mirror=off` (B-D11), and `land_default_channel` = `#general`. An existing channel that mirrors to Slack fails the step. A post in `#support` becomes a support bead and an operator DM ([support-runbook.md](support-runbook.md)). |
| `namespace` | Applies the land's namespace `land-<land>` from the §5.2 template (`internal/lands/landns`, step 10.2) and sets the project's `namespace` and `runtime_class` (`LANDS_RUNTIME_CLASS`, chart `landNamespaces.runtimeClassName`; empty by default, the node's default runtime). With `LANDS_NAMESPACES_ENABLED` off the step fails: a land never opens without its fence. |
| `steward` | Binds the land's steward (`internal/steward` `ProvisionHook`, step 9.3; [steward.md](steward.md)) with the approver as its operator, then wakes it once with a `land_opened` notice in `#machines`, which its `land-opened` wake entry fires on. The wake waits until the steward trigger is `StewardSettle` (2 minutes) old, so trigger-engine (30s poll) has loaded it: the pass that binds ends there without failing, and a later pass posts the wake. The steward's first run greets `#general`. |
| `mail` | Stamps `land_opened_at`, mails "your land is ready", and sets the application `provisioned` with `project=<land>`. |

Tenant spawns off (`LANDS_TENANT_SPAWNS` unset, the private beta's setting) does not stop the
steward: with them off a tenant land runs its steward and nothing else (owner ruling
gd-KOAoUELMWo). The spawn wall lets exactly the `steward` role past `tenant_spawns_off` and
refuses its gardeners and every other role. Every other wall refusal still stops the steward: a
frozen land, the `lands-pause` gate (open or unreadable), a land gone, and a purse at the wall or
unknown. When one of those refuses the opening wake, the greeting waits for the land's next wake (a
person's message in `#general`, or the daily digest).

**Failure.** A failed step sets the application `failed` with `provision_error` (`<step>: <error>`),
`provision_failed_at` and `provision_escalation_id`, and files one escalation (operation type
`lands_provision`, label `project:lands-intake`, `requesting_task` = the application, keyed by the
attempt). Nothing is rolled back. Fix the cause, then:

```sh
gb lands provision <application|land>          # where it is: state, step, attempt, error
gb lands provision <application|land> --retry  # failed → provisioning, attempt + 1
```

### Releases

A land's releases ([`releases-design.md`](../releases-design.md) §4) are switched on by three
fields on its project bead: `release_tag_pattern` (a Go regexp a tag must match; unset means
the land has no releases), `release_targets` (a JSON list of `{name, kind}`, the places it
runs) and `release_branch` (optional; default `default_branch`). The `project` step does not
set them yet, so a new land has no releases. An admin switches them on with the land's
defaults, semver and one `prod` target:

```sh
kd update <land-project-bead-id> \
  -f release_tag_pattern='^v\d+\.\d+\.\d+$' \
  -f release_targets='[{"name":"prod","kind":"land"}]'
```

The fields are not in the set `BEADS_LANDS_OPERATOR_GITHUB_ORGS` freezes, so an admin can change
a land's pattern later. Releases are read with `kd release list --project <land>`.

### A closed steward

No open land goes without an open steward (`internal/lands/provision/rebind.go`). Each pass looks
at every `provisioned` land that is not being closed and not frozen, and when its steward trigger
is closed, binds a fresh one (`Bind` adopts only an open steward, so it mints) with the approver as
operator, and comments why on the land's project bead. The fresh steward is not woken: it wakes on
the land's next event or its daily digest, and `steward-notes:<land>` carries what the old one knew.

- **Closed by hand.** Bound again on the next pass. To keep a land without a steward, freeze it
  (`gb lands freeze <land>`); a frozen land is left alone.
- **Crash loop.** A steward the engine closed on a hard stop other than its wall clock
  (`gardener_terminated_slot=hard_stop`: `consecutive_failures`, or the credential crash-loop gate)
  is not bound again for `StewardRebindBackoff` (24h). The pass files one escalation instead
  (operation type `lands_steward`, label `project:lands-intake`, keyed on the closed steward). Fix
  the cause and close the escalation: the next pass binds a fresh steward. Left open, a fresh one
  is bound when the 24 hours are up.

A land with a close requested, or closed, is never bound a steward: the close's `steward` step retires it.

## Closing

`gb lands close <land> --reason "<text>"` writes `close_requested_at`, `close_requested_by` and
`close_reason` on the application (approved, provisioned or failed; a `provisioning` one is refused
until it finishes). The next pass runs, recording each step in `close_step`:

| Step | What it does |
|---|---|
| `freeze` | `land_frozen=true` on the project, so the spawn wall refuses every spawn (§3.5). |
| `steward` | Retires the land's steward trigger (`ProvisionHook.Retire`): `enabled=false`, so the trigger-reviver never reads it as a dark lane, then closes it, so trigger-engine stops loading it and its wakes and daily digest stop firing. A controller with no steward hook fails the step unless provisioning never reached the steward step. |
| `agents` | `agent_state=stopping` on every live agent bead of the land (the `gb stop` path), with a comment. |
| `members` | Everyone in the land's groups leaves them; a user who then holds no land (`UserLands`) leaves `lands-tenants`. The empty group is kept. |
| `key` | Revokes the land's LiteLLM key, clears `litellm_key` and closes `lands-<land>`. |
| `project` | Stamps `land_closed_at` and closes the project bead. |
| `mail` | Mails "land closed" (only to a land that was provisioned) and sets the application `closed`. |

The land namespace joins between `key` and `project` with step 10.2. Beads are kept until the purge
sweep below deletes them after the retention period. A project of the land's name that another application
made is never touched: the close fails at `freeze`. A failed close files a `lands_close` escalation
and waits; run `gb lands close` again to resume it from `close_step`.

## Purging (step 8.11)

The purge sweep (`internal/lands/purge`, `cmd/controller/lands_purge.go`) makes the privacy
policy's §5 deletion promises true. It runs hourly and is off unless `LANDS_PURGE_ENABLED=true`.
On, it is a dry run unless `LANDS_PURGE_DRY_RUN=false`: it deletes and stamps nothing and logs,
for each land, ledger and application that is due, what would go (`lands purge: dry run: land
due`, with the bead, agent and session-object counts). Read the first dry run's log before
setting `false` in a separate roll.

| When | What goes |
|---|---|
| `land_closed_at` + `LANDS_RETENTION_DAYS` (30) | The coop session objects of every agent bead the purge deletes (`<COOP_S3_PREFIX>/<agent bead id>/`, every version and delete marker), then every bead labelled `project:<land>`, soft-deleted ones too, through `POST /v1/lands/purge`. Kept: the project bead (a tombstone that keeps the name and bead prefix taken), the `credit_grant` ledger, and any bead that also carries another project's label. The project gets `land_purged_at`. |
| `land_closed_at` + `LANDS_RECORDS_RETENTION_DAYS` (730) | The `credit_grant` ledger; the project gets `land_records_purged_at`. |
| decision (`decided_at`, else close) + `LANDS_APPLICATION_RETENTION_DAYS` (90) | A `denied` or `withdrawn` `plot_application`. The application of a land that opened is its agreement record and stays. |

The daemon's purge is a hard delete: the bead row, its labels, deps, comments, embeddings,
followers and taints, and its `events`, `mutation_history`, `slow_tool_calls` and inbound
`bead_references` rows. It refuses anything but a closed tenant land with `land_closed_at`, or a
denied/withdrawn application, and admits only an OIDC admin or a machine principal holding
`gardenio-machine-lands-provisioner` (capability `land_purge`), never the static token.

The daemon holds every purge, dry runs included, to its own retention floors:
`BEADS_LANDS_RETENTION_DAYS` (30) after `land_closed_at` for the land,
`BEADS_LANDS_RECORDS_RETENTION_DAYS` (730) for the ledger, and
`BEADS_LANDS_APPLICATION_RETENTION_DAYS` (90) after `decided_at` (else close, else last update) for
an application. A request inside the floor gets 409 naming the date it may go. The controller's
`LANDS_*_RETENTION_DAYS` may be longer than the daemon's, never shorter: a shorter one is refused
every pass, before any session is deleted. An unset or invalid floor is the default, so a typo
never shortens the policy.

Session objects are found only by agent bead ids, never by an agent's `coop_session_id` or
`s3_session_key`: the agent's own pod writes those (`gb init` adopting a resumed chain), so a tenant
could point them at an operator agent. A resumed chain's key is an earlier agent of the same land,
whose id is already in the list. A field that names any other key is logged (`agent names a session
key outside the land; not deleted`) for an operator to look at.

Sessions go first, because the agent list they are found by goes with the beads; a land is stamped
only once both are gone, and a failure is retried on the next pass. The sweep does not start if
`COOP_S3_BUCKET` is set but the controller cannot load S3 credentials; it needs
`s3:ListBucketVersions` and `s3:DeleteObjectVersion` on the session prefix.

Not done by the sweep, and needed before the policy is published (legal README "Before
publishing"):

- **LiteLLM spend rows.** LiteLLM has no API that deletes a key's rows. They age out under its
  global `maximum_spend_logs_retention_period`, which must be no longer than
  `LANDS_RETENTION_DAYS`.
- **Postgres backups.** The hourly `s3Backup` dumps hold every bead; their bucket needs an expiry
  of at most 35 days.

## Sign-in invites (gd-ycE484kLyk)

GitHub sign-in is invite-only (owner ruling gd-hCMlZ5jnvB = b): `gardenio.clonk.tv` admits `/lands/*`
only to `/invited`, `/lands-tenants`, `/gardenio-admins` and the deploy-values project groups. The
one exception is the demo land's door, `/lands/demo` and the terms page `/lands/accept`, which admit
any signed-in account (gd-T37nCeWMhq = open; [demo-land.md](demo-land.md)). An
admin invites someone by email:

```sh
gb lands invite someone@example.com                       # sign in only
gb lands invite someone@example.com --land acme --role guest
gb lands invites [--all]                                  # what is open
gb lands uninvite <invite-id>                             # end a pending one
```

That files a `type=invite` bead (admin-gated; rules in `kbeads/internal/server/invite.go`). Every
pass, after the member invites, the provisioner (`email_invites.go`):

1. **Mails** a pending invite with no `mailed_at` (template `invite`: who invited them, a link to
   `/lands/`, "sign in with GitHub using this email"), then stamps `mailed_at`. With mail off nothing
   is stamped, so it goes out once mail is on; a failed send is retried at most hourly.
2. **Claims** it once exactly one Keycloak user has exactly that email, `emailVerified=true`, and a
   `github` identity link: they join `invited` (and the land with the role, plus `lands-tenants`,
   when the invite names one; the gatehouse as a guest when it names none and they hold no land,
   [gatehouse.md](gatehouse.md)), and the invite becomes `claimed` and closes. A person who joined
   a land so is greeted by its steward in `#general` (step 9.14, [steward.md](steward.md)). An unverified address,
   a user with no GitHub link, a different address or two users with it: nobody gets in, and the
   invite waits. The realm verifies every GitHub registrant's address (`login.verifyEmail`, and the
   github IdP never trusts GitHub's), so the match is to an address the person proved they hold.
   Nothing here links accounts or touches the GitHub first broker login flow.
3. **Ends** a pending invite at `expires_at` (14 days) or on `gb lands uninvite`.

A claimed invite is terminal. To take access away, remove the person from `invited` (and any land)
in Keycloak.

## Configuration (controller)

| Variable | Meaning |
|---|---|
| `LANDS_PROVISION_ENABLED` | `true` turns the loop on. Off by default. |
| `LANDS_KEYCLOAK_URL`, `LANDS_KEYCLOAK_REALM` (default `gardenio`), `LANDS_KEYCLOAK_CLIENT_ID` (default `gardenio-lands-provisioner`), `LANDS_KEYCLOAK_CLIENT_SECRET_PATH` | The Keycloak admin client ([keycloak-provisioner.md](keycloak-provisioner.md)). The secret is read from a mounted file, never the environment. |
| `LANDS_KEYCLOAK_RESERVED_GROUPS` | Comma-separated groups the deployment renders besides `keycloakadmin.ReservedGroups`. |
| `LANDS_GITHUB_APP_ID`, `LANDS_GITHUB_APP_PRIVATE_KEY_PATH` | The Lands GitHub App ([github-app.md](github-app.md)), for default branches. |
| `LANDS_OPERATOR_GITHUB_ORGS` | Comma-separated operator GitHub accounts. The controller never mints or refreshes a Lands token for a repo one of them owns, or from an installation on one. |
| `LITELLM_URL`, `LITELLM_MASTER_KEY` | Already set for spend sync. |
| `LANDS_ACCOUNT_TEMPLATE` | The account bead whose provider, base URL, credential and model tenant lands' accounts copy (the fleet Claude credential stays, owner ruling gd-NXTrUXD1Sq; a land may be repointed at its own `secret:lands-<land>`). |
| `MAILER_*` | Mail, off by default (`internal/mailer`). |
| `LANDS_PURGE_ENABLED` | `true` turns the purge sweep on. Off by default. |
| `LANDS_PURGE_DRY_RUN` | Anything but `false` (unset included) makes the sweep log what is due and delete nothing. |
| `LANDS_RETENTION_DAYS`, `LANDS_RECORDS_RETENTION_DAYS`, `LANDS_APPLICATION_RETENTION_DAYS` | The sweep's periods in days: 30, 730 and 90 by default (the privacy policy's `{{RETENTION_DAYS}}`, `{{RECORDS_RETENTION}}`, `{{APPLICATION_RETENTION}}`). Records may not be shorter than retention. The daemon's `BEADS_LANDS_*_RETENTION_DAYS` (same defaults) are the floor. |
| `LANDS_GATEHOUSE_LAND` | The gatehouse ([gatehouse.md](gatehouse.md)): the land every signed-in person with no land is a guest of. Unset, none. Must differ from `LANDS_DEMO_LAND`. |
| `COOP_S3_BUCKET`, `COOP_S3_PREFIX`, `COOP_S3_REGION`, `COOP_S3_ENDPOINT` | Where the sweep deletes session objects; already set for session persistence. |

On but missing any of these, the provisioner does not start and logs which; approved applications
wait. Its daemon writes go out as the controller's machine principal, which must hold
`gardenio-machine-lands-provisioner` (kbeads `machine_grants.go`: update `plot_application` with
typed ownership; create/update/close `project` and `account`; create `credit_grant`; and the
`land_purge` capability the sweep uses). The sweep alone also needs that role.

### Chart values

The chart renders these from `agents.lands.provisioner` (all off by default; disabled renders no
env and no volumes):

| Value | Renders |
|---|---|
| `enabled` | `LANDS_PROVISION_ENABLED=true`, and everything below. |
| `keycloak.url` | `LANDS_KEYCLOAK_URL`. Must be `https://` (the render fails otherwise, as keycloakadmin would). Empty uses the keycloak-runtime Service over in-cluster TLS, `https://<release>-keycloak-runtime.<ns>.svc.cluster.local:<httpsPort><httpRelativePath>` (on k3s `https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth`), which needs `keycloak.runtime.enabled` and `keycloak.runtime.tls.enabled`; required otherwise. With `keycloak.runtime.tls` on, the controller mounts the CA (`ca.crt` only, at `keycloak.caMountPath`) and sets `LANDS_KEYCLOAK_CA_FILE` (gd-XRsosVFAau). With `networkPolicies` on, the chart opens controller → keycloak-runtime on the https port. |
| `keycloak.realm` | `LANDS_KEYCLOAK_REALM`; empty uses `keycloak.realm.name`, then the release name. |
| `keycloak.clientId` | `LANDS_KEYCLOAK_CLIENT_ID` (default `gardenio-lands-provisioner`). |
| `keycloak.clientSecretSecret`, `keycloak.mountPath` | Secret `gardenio-lands-provisioner` key `clientSecret`, mounted read-only; `LANDS_KEYCLOAK_CLIENT_SECRET_PATH=<mountPath>/clientSecret`. |
| `keycloak.extraReservedGroups` | Added to `LANDS_KEYCLOAK_RESERVED_GROUPS`, which already lists every `keycloak.groups` name and `project-<keycloak.projectGroups name>`. |
| `githubApp.appId`, `githubApp.apiUrl` | `LANDS_GITHUB_APP_ID` (required), `LANDS_GITHUB_APP_API_URL`. |
| `githubApp.privateKeySecret`, `githubApp.mountPath` | Secret `gardenio-lands-github-app` key `private-key`, mounted read-only; `LANDS_GITHUB_APP_PRIVATE_KEY_PATH=<mountPath>/private-key`. |
| `accountTemplate` | `LANDS_ACCOUNT_TEMPLATE` (required). |

Both Secrets are mounted `optional`, so a missing one still starts the controller; the provisioner
logs the file it could not read and stays off. `agents.lands.tenantSpawns` and
`agents.lands.spawnsPerHour` render `LANDS_TENANT_SPAWNS` and `LANDS_SPAWNS_PER_HOUR`, and
`agents.mailer.*` renders `MAILER_*` (`from` and `baseUrl` are required when it is enabled).

`agents.lands.operatorGithubOrgs` (default `mattbakerconsulting`, `joyousaversion`; list every org
the operator owns, the ClaimLayer org included, in the deploy values) is the operator's own GitHub
accounts (gd-72m3STiqya). A land that mints with the Lands App is never bound to a repo they own:
the beads daemon refuses that `git_url`, `repos`, `github_app`, `github_installation_id` or
`land_kind` write from anyone, admin included (`BEADS_LANDS_OPERATOR_GITHUB_ORGS`, rendered whenever
the list is non-empty), and the controller refuses the mint and refresh
(`LANDS_OPERATOR_GITHUB_ORGS`, rendered with the provisioner block, which wires the Lands App).
While the list is set, the single-field endpoints refuse those keys on a project bead; write them
with an update. Tests: `gardenio/helm/gardenio/tests/lands_operator_orgs_test.go`. Neither
Secret may be mirrored into the gVisor namespace; the render fails if one is. Tests:
`gardenio/helm/gardenio/tests/lands_provisioner_test.go`.

## The viewer edge

`gardenio.clonk.tv`'s sign-in check is a static group list, so a land created at runtime is in none
of it (gd-A9avO8cXBr). The provisioner takes option (1) from that bead: it never writes edge config.
Every tenant joins the static `lands-tenants` group, which the edge admits (step 7.8), and kbeads and
the viewer do the per-land scoping from `/project-<land>[/owners|/guests]`, as they already do. `^~ /lands/` has its own check, `/oauth2/auth-lands`: the same list plus `/invited` (sign-in
invites above). `= /lands/demo` and `= /lands/accept` use `/oauth2/auth-lands-demo`, which has no
group list, so a stranger can take a demo pass. `scripts/check-viewer-edge-groups.py` holds both lists
to deploy-values and allows the open check on those two locations only.
