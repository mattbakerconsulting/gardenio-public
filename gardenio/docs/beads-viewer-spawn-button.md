# beads-viewer: replace "Claim" button with "Spawn"

Tracking: ka-0000008202. Slack thread: Matt asked for this; "claim doesn't really
make sense, it should be a 'spawn' button".

## Why

The viewer is a human-operated UI. The current **Claim** button calls
`UpdateBead(assignee=operator_email, status=in_progress)` — copying the
`kd claim` semantics agents use. That makes sense for an agent at the CLI
that is about to do the work itself, but a human in the browser is **not**
going to do the work; they want an agent to. So claiming a bead from the
viewer just orphans it under the operator's email until someone notices.

The right primitive from the viewer is **Spawn**: create an agent bead,
auto-named from the task, assigned to this task, attributed to the operator.

## What ships

- A **Spawn** button replacing **Claim** on:
  - `templates/partials/bead_actions.html` (bead-show toolbar)
  - `templates/partials/active_row.html` (active beads list)
- A `POST /b/{id}/spawn` route that calls `daemon.SpawnAgent(...)` with
  fields derived from the target bead and operator identity.
- The button is shown on **open or in_progress** beads of type
  `task | bug | epic | chore` that the operator does **not** already
  have an active agent on. Closed beads, agent beads, advice/decision/
  schedule beads do not show Spawn.
- **Release** stays. It's the "the operator wants to step away from a bead
  they took ownership of" action — still useful for beads that legitimately
  ended up with an operator email as assignee.
- **Claim** goes away from the UI entirely. Behind the scenes the
  `POST /b/{id}/claim` route can stay for a release, then be deleted, OR
  be deleted immediately — it has no remaining caller in the viewer
  templates. Recommend: delete the route + handler in the same MR (no
  external callers).

## Server changes

### `bead_actions.go`

Extend `beadActionsView`:

```go
type beadActionsView struct {
    // ... existing fields ...
    CanSpawn       bool   // replaces CanClaim
    SpawnedAgentID string // populated after a successful Spawn — drives
                          // a "Spawned agent <name>" inline confirmation
}
```

`buildBeadActions(bead, email)`:

- Drop `CanClaim` / Claim logic.
- Add `CanSpawn` rule:
  ```
  CanSpawn = email != "" &&
             bead.Status in {"open","in_progress"} &&
             bead.Type in {"task","bug","epic","chore"} &&
             projectFromLabels(bead.Labels) != ""
  ```
  (project is required to spawn; if a bead is unlabeled, the Spawn button
  is hidden rather than failing at click time.)

Drop `handleClaimBead` and the `POST /b/{id}/claim` route registration.

Add `handleSpawnAgent`:

```go
func (s *Server) handleSpawnAgent(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    email := oidcauth.EmailFromRequest(r)
    if email == "" { http.Error(w, "Unauthenticated", 401); return }

    bead, err := s.daemon.GetBead(r.Context(), id)
    if err != nil { /* 404 / 500 like runBeadAction */ return }

    project := projectFromLabels(bead.Labels)
    if project == "" {
        s.renderBeadActionsPartial(w, r, bead, email,
            "Cannot spawn: bead has no project label.", 400)
        return
    }

    agentName := generateAgentName(project, bead.Title) // shared helper
    role := "crew"
    extras := map[string]string{"spawned_by": email}

    agentID, err := s.daemon.SpawnAgent(r.Context(),
        agentName, project, bead.ID, role, "" /* customPrompt */, extras)
    if err != nil {
        s.renderBeadActionsPartial(w, r, bead, email,
            "Failed to spawn agent: "+err.Error(), 502)
        return
    }

    updated, _ := s.daemon.GetBead(r.Context(), id)
    if updated == nil { updated = bead }

    // Render with a SpawnedAgentID so the toolbar shows confirmation.
    view := buildBeadActions(updated, email)
    view.SpawnedAgentID = agentID
    s.renderBeadActionsPartialView(w, r, view, "", 200)
}
```

Register: `mux.HandleFunc("POST /b/{id}/spawn", s.handleSpawnAgent)`.

### `beadAPI` interface (server.go)

Add:

```go
SpawnAgent(ctx context.Context, agentName, project, taskID, role,
    customPrompt string, extraFields ...map[string]string) (string, error)
```

`beadsapi.Client` already implements this (controller and `gb spawn` both
call it), so the only change is exposing it on the `beadAPI` interface and
the `fakeDaemon` in tests.

### Shared agent-name helper

`spawnGenerateAgentName(project, title)` and `spawnRandomSuffix(n)` currently
live in `gardenio/controller/cmd/gb/spawn.go` (package `main`). Move to a
new package, e.g. `gardenio/controller/internal/agentname/`:

```text
internal/agentname/
    name.go          # GenerateAgentName, RandomSuffix, IsValidAgentName
    name_test.go
```

Update `cmd/gb/spawn.go` and `cmd/beads-viewer/bead_actions.go` to import it.
This is a small refactor but it keeps the slug rules consistent across
spawn entry points.

## Template changes

### `bead_actions.html`

Replace the `{{if .CanClaim}}…Claim button` block with:

```html
{{if .CanSpawn}}
  <button type="button"
          class="bead-action bead-action-spawn"
          hx-post="{{$.BasePath}}/b/{{.BeadID}}/spawn"
          hx-target="#bead-actions"
          hx-swap="outerHTML"
          hx-confirm="Spawn an agent on {{.BeadID}}?"
          hx-disabled-elt="this">Spawn</button>
{{end}}
{{if .SpawnedAgentID}}
  <p class="bead-actions-confirmation" role="status">
    Spawned <a href="{{$.BasePath}}/{{.SpawnedAgentID}}">{{.SpawnedAgentID}}</a>
  </p>
{{end}}
```

`hx-confirm` matches the existing UX (Close has it). Confirmation copy is
deliberately specific — spawning an agent costs real money and triggers a
pod, so a one-click no-confirm Spawn is too easy to fat-finger.

### `active_row.html`

Same swap: drop `CanClaim`/Claim block, add `CanSpawn`/Spawn block with
`?return=row` and `hx-target="#bead-row-{{.BeadID}}"` to keep the row-swap
behavior.

### CSS

Add `.bead-action-spawn` to the existing button styles. Visually
distinguish it from Close/Release (e.g. accent color) since it's a
**create** action, not a state-change action. CSS lives in
`gardenio/controller/cmd/beads-viewer/static/`.

## Tests

Mirror the existing `bead_actions_test.go` pattern.

1. **Toolbar render** — `TestBeadShow_ActionsToolbar_AuthedSpawn`:
   open task with project label, no operator-owned agents → Spawn button
   present, Claim absent.
2. **No project label** → `CanSpawn = false`, no Spawn button, no error.
3. **Closed / agent / decision bead types** → Spawn absent.
4. **POST /b/{id}/spawn**:
   - Success: 200, response includes `Spawned <agentID>` confirmation,
     fakeDaemon recorded a SpawnAgent call with correct args.
   - Unauthenticated: 401.
   - Daemon error: renders error in toolbar at 502 (matches the existing
     `runBeadAction` pattern).
   - `?return=row`: swaps the active-row partial.
5. **fakeDaemon** gets a `SpawnAgent` method that records the call and
   returns a stub bead ID, mirroring how the existing close/claim tests
   exercise `UpdateBead`.

## E2E / screenshot impact

`gardenio/tests/beads-viewer-e2e/` and `…-screenshots/` — audit any spec
that asserts the Claim button text or class. Update screenshot baselines
as part of the same MR; the screenshot diff is the human-readable proof
the swap landed cleanly.

## Out of scope (separate beads)

- **Role override / prompt customization at spawn time.** The first cut
  hardcodes role=crew with no custom prompt. A modal that lets the
  operator pick role and dictate a prompt is a larger UX change and
  should land on its own bead once the basic Spawn button is shipped
  and exercised.
- **beads3d** has its own context-menu Claim action
  (`beads3d/src/context-menu.js:84`). Same critique applies but the JS
  app's mutation flow is different (optimistic local update + RPC). File
  a sibling bead to swap that for Spawn once the daemon-side flow is
  proven; do **not** bundle into this MR.
- **Spawn quotas / rate limiting from the viewer.** The daemon already
  enforces spawn authorization via `authorizeAgentSpawn` (see
  `bridge/trigger_permissions.go`). The viewer just forwards. If we
  later want browser-level rate limiting (operator-spam protection),
  that's its own bead.

## Steps to land

1. Move spawn name helpers into `internal/agentname/` + tests.
2. Add `SpawnAgent` to the `beadAPI` interface and `fakeDaemon`.
3. Replace `CanClaim` with `CanSpawn` in `beadActionsView`,
   `buildBeadActions`, both templates.
4. Add `handleSpawnAgent` + `POST /b/{id}/spawn` route. Delete
   `handleClaimBead` + `POST /b/{id}/claim` route.
5. Update tests in `bead_actions_test.go`.
6. Audit + update Playwright e2e + screenshot specs.
7. `make test`, `make lint`, `quench check`, `helm lint helm/gardenio/`.
8. MR: `feat(beads-viewer): replace Claim with Spawn (ka-0000008202)`.

## Risk / things to watch

- **Permission denials.** A viewer operator without spawn scope will get
  a daemon-side 403. The plan's error rendering surfaces that inline; we
  do **not** silently no-op. Test covers this case.
- **Dedup behavior.** `SpawnAgent` already has a name-based dedup guard
  (`findActiveAgentByName`). If an operator double-clicks Spawn on the
  same bead within seconds, the second call returns the same agent ID.
  `hx-disabled-elt="this"` should make this rare.
- **Project-less beads.** Some beads in the wild have no `project:` label.
  Hiding Spawn on those is the correct fail-closed behavior; don't
  default to a bare-name agent that the controller can't schedule.
