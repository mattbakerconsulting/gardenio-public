# Lands contract: leaving v1-beta

The contract ships as `1.0.0-beta.N` (`info.version` in `openapi.yaml`,
`landsv1.ContractVersion`). This page is the written exit test: what must hold
before the `-beta` suffix comes off and `1.0.0` is cut, how each part is
measured, and who decides.

Status: **proposed** (task gd-VMGMkafIBm, contract design gd-EWSKPH7pa5 §7 and
§8 step 10). The owner answers design question 8 ("Who signs off on v1-beta
exit, and is 'two tenants, one month' the criterion?") by accepting or
amending this page at Open day 11.4.

## What the beta covers

The resources `1.0.0` would freeze, with the beta version that added each
(README, "Decisions made here that the design left open"):

| Resource | Operations | Since |
|---|---|---|
| Land, plots, crew, whoami | read | 1.0.0-beta.1 |
| Tickets (comments, close, reopen, search) | read, write | 1.0.0-beta.1 |
| Decisions | read, resolve, cancel, comment | 1.0.0-beta.1 |
| Escalations | read, open | 1.0.0-beta.1 |
| Plans | read, answer | 1.0.0-beta.1 |
| Threads and messages | read, post (answered `not_found` until D.9) | 1.0.0-beta.1 |
| Budget | read | 1.0.0-beta.1 |
| Heraldry on tickets and plans | read, write | 1.0.0-beta.2 |
| Budget history | read | 1.0.0-beta.3 |
| Decision option fields and evidence | read | 1.0.0-beta.4 |
| Agents on a member's own machine | read, start, refresh, stop | 1.0.0-beta.4 |
| Releases (contents, deployments) | read; report a deployment (owner only) | 1.0.0-beta.5 |
| Events (`gl watch`) | stream; `budget.updated` since 1.0.0-beta.3, `release.cut` and `release.deployed` since 1.0.0-beta.5 | 1.0.0-beta.1 |

A resource added during the window is a new operation, not a break (below),
so it does not restart the clock; it is frozen with the rest at the cut.

## What exit means

Leaving beta changes no rule in `openapi.yaml`'s `info.description`: the
contract is already additive-only (README, "Changing the contract"). Exit is a
promise about time: after `1.0.0`, a client built against `1.0.0` keeps working
until a `/lands/v2/` exists beside it. During beta the owner may still make a
breaking change, at the price of restarting the clock below.

## The criteria

All five must hold at once. The window is one calendar month (30 days) and
resets to zero whenever criterion 3 or 4 fails.

| # | Criterion | Holds when | Measured by |
|---|---|---|---|
| 1 | **Two tenants** | Two lands owned by people outside the fleet's operators (tenant lands, each with a purse) are in use. Operator lands and test lands do not count. | The owner names the two lands when the window opens. |
| 2 | **One month** | Members of each of the two lands call the Lands API (`gl` or the web UI) in each of the four weeks of the window. | The daemon's `gardenio_lands_api_requests_total`, read at the end of each week: `sum(increase(gardenio_lands_api_requests_total{land="<name>",caller="member"}[7d])) > 0`. `land` is set only for a land the map admitted the caller to; `caller="member"` is a land member's sign-in, so the land's own agents (`caller="agent"`) and the fleet's operators (`caller="operator"`) do not count as its use. |
| 3 | **Zero contract breaks** | No break (defined below) reaches `main` or a tenant during the window. | Every `main` commit in the window is green in `checks.yml` job `lands-contract` and `ci.yml` job `lands-conformance`, which replays the request fixtures and the scenario fixtures (`scenarios/`) against a real `kd serve`. Any tenant report of a changed answer is triaged against the definition below. |
| 4 | **Zero denylist hits** | No structural hit in any Lands API answer, recorded or live, and every text hit reviewed. | The same CI runs: `TestHygieneDenylist` (this module), `TestResponseHygiene_Golden` (kbeads/internal/landmap) and the conformance suite's `hygiene` step over every answer the land door forwarded. Live answers: the daemon's `gardenio_lands_denylist_hits_total`, which the map counts over every answer it writes and every land feed frame. `increase(gardenio_lands_denylist_hits_total{kind="structural"}[30d]) == 0` over the window; each `kind="text"` hit is read in the daemon log (`landmap: denylisted answer`, by rule, op, land and correlation id, never the answer) and triaged. See "Structural and text hits". |
| 5 | **Owner sign-off** | The owner, having read the evidence for 1-4, approves the cut. | A decision bead on the plan (gd-vmw5s07A1F) resolved by the owner; the cut commit names it. |

### What counts as a break

Any of these, on a path under `/lands/v1/`:

- a response field removed, renamed or retyped, or a field that was always
  present becoming absent;
- an enum value removed, or a value an old client would read as `other`
  replacing one it knew;
- a request field becoming required, or an accepted request now refused;
- an operation removed, or answering a different status for the same request;
- a scenario fixture's answer changed for the same given, other than by adding
  a field or a scenario (`scenarios/`: what an answer means, not only its
  shape);
- a `gl` built from the window's first commit refusing, or warning of skew,
  against the window's last.

Adding a field, an enum value, an operation, a fixture or a scenario is not a
break. Neither is a fix that makes an answer match a scenario it failed, if the
scenario was in place before the window opened.

### Structural and text hits

The live counter (gd-AQQaiE8IMd) labels each hit by `kind`:

- **structural**: a daemon field key, a bead type the contract has no word
  for, a daemon route or a header outside the contract's set. These are leaks
  of the map's own; one is a break of criterion 4.
- **text**: a `hygiene_denylist.txt` line matched inside free text. Land
  agents write "bead" and "daemon" in titles and comments, and the prose scrub
  (`kbeads/internal/landmap/scrub.go`) rewrites ids, daemon paths and service
  hosts but not plain words, so text hits are expected. Each is reviewed: a
  tenant's own prose is not a leak; anything the fleet wrote that names its
  internals is.

Both counters are per daemon process; `increase()` reads across restarts.

## Evidence at sign-off

The operator assembles, for the owner:

1. the two lands and their weekly call counts (criteria 1 and 2);
2. the `main` commit range of the window and the `lands-contract` and
   `lands-conformance` results for each commit (criteria 3 and 4);
3. the diff of `openapi.yaml` and `scenarios/` across the window, which must be
   additive;
4. any tenant reports in the window and how each was triaged.

## The cut

1. Set `info.version` to `1.0.0` and `landsv1.ContractVersion` with it.
2. `make generate-lands-contract && make test-lands-contract`.
3. Update the README's title (drop "(v1-beta)") and the "Decisions made here"
   list, which then becomes the v1 record.
4. One commit naming the owner's decision; release as usual.
