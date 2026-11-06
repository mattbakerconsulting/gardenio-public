package bridge

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gateDep is a tiny constructor for a forward blocking dependency
// (task --type--> blocker) used across the table cases. (Named to avoid
// colliding with the package-level `dep` helper in subgraph_resolver_test.go,
// which has a different argument order.)
func gateDep(beadID, depType, dependsOn string) beadsapi.Dependency {
	return beadsapi.Dependency{BeadID: beadID, Type: depType, DependsOnID: dependsOn}
}

func TestEvaluateDispatchGate(t *testing.T) {
	t.Run("no linked task → dispatch (fail-open)", func(t *testing.T) {
		got := EvaluateDispatchGate(DispatchGateInput{})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true for empty input, got false; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 0 {
			t.Errorf("BlockerIDs = %v, want empty", got.BlockerIDs)
		}
	})

	t.Run("task with no deps → dispatch", func(t *testing.T) {
		got := EvaluateDispatchGate(DispatchGateInput{TaskID: "gd-task"})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true, got false; reason=%q", got.Reason)
		}
	})

	t.Run("MR !6218 repro — task blocked by open decision AND open escalation → SUPPRESS", func(t *testing.T) {
		// The exact shape of bug ka-0000004390: task ka-0000001997 carries
		// forward `blocks` edges to open decision gd-Kb9MFSsoLq and open
		// escalation ka-0000007362. Six+ Opus review-merge crews were dispatched
		// in ~18h; every one is redundant because the sole remaining action is a
		// human merge/hold call.
		in := DispatchGateInput{
			TaskID: "ka-0000001997",
			Deps: []beadsapi.Dependency{
				gateDep("ka-0000001997", "produces", "gd-a0cs85pSFf"),
				gateDep("ka-0000001997", "blocks", "gd-Kb9MFSsoLq"),
				gateDep("ka-0000001997", "blocks", "ka-0000007362"),
				gateDep("ka-0000001997", "parent-child", "ka-0000004689"),
			},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-Kb9MFSsoLq": {ID: "gd-Kb9MFSsoLq", Type: "decision", Status: "open"},
				"ka-0000007362": {ID: "ka-0000007362", Type: "escalation", Status: "open"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (parked on operator merge-gate), got true; reason=%q", got.Reason)
		}
		// Both handles reported, sorted for determinism.
		want := []string{"gd-Kb9MFSsoLq", "ka-0000007362"}
		if len(got.BlockerIDs) != len(want) {
			t.Fatalf("BlockerIDs = %v, want %v", got.BlockerIDs, want)
		}
		for i := range want {
			if got.BlockerIDs[i] != want[i] {
				t.Fatalf("BlockerIDs = %v, want %v (sorted)", got.BlockerIDs, want)
			}
		}
	})

	t.Run("blocked by open decision only → SUPPRESS", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-dec")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-dec": {ID: "gd-dec", Type: "decision", Status: "open"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true; reason=%q", got.Reason)
		}
	})

	t.Run("blocked by open escalation only → SUPPRESS (the gap the old bash DEDUP missed)", func(t *testing.T) {
		// The prompt-driven DEDUP scanned only type=decision via --mentions, so
		// an MR parked purely on an escalation slipped through every cycle. The
		// deterministic gate catches it.
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-esc")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-esc": {ID: "gd-esc", Type: "escalation", Status: "open"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 1 || got.BlockerIDs[0] != "gd-esc" {
			t.Errorf("BlockerIDs = %v, want [gd-esc]", got.BlockerIDs)
		}
	})

	t.Run("blocker CLOSED → dispatch (gate cleared, crew may merge)", func(t *testing.T) {
		// Operator resolved the decision/escalation → the block edge is history,
		// not a wait edge, so the MR is now genuinely actionable.
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps: []beadsapi.Dependency{
				gateDep("gd-task", "blocks", "gd-dec"),
				gateDep("gd-task", "blocks", "gd-esc"),
			},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-dec": {ID: "gd-dec", Type: "decision", Status: "closed"},
				"gd-esc": {ID: "gd-esc", Type: "escalation", Status: "closed"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (blockers closed), got false; reason=%q", got.Reason)
		}
	})

	t.Run("in_progress / blocked / deferred blocker → SUPPRESS (non-terminal statuses)", func(t *testing.T) {
		for _, status := range []string{"in_progress", "blocked", "deferred"} {
			in := DispatchGateInput{
				TaskID: "gd-task",
				Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-dec")},
				Blockers: map[string]*beadsapi.BeadDetail{
					"gd-dec": {ID: "gd-dec", Type: "decision", Status: status},
				},
			}
			got := EvaluateDispatchGate(in)
			if got.MayDispatch {
				t.Errorf("status=%q: expected MayDispatch=false, got true", status)
			}
		}
	})

	t.Run("open blocker of a NON-gate type → dispatch (a crew can still unblock)", func(t *testing.T) {
		// Blocked by a sibling TASK, not an operator handle: a crew could still
		// advance it, so the MR walk must not suppress on it.
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-sib")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-sib": {ID: "gd-sib", Type: "task", Status: "open"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (blocker is a task, not a gate), got false; reason=%q", got.Reason)
		}
	})

	t.Run("open decision linked by a STRUCTURAL edge (parent-child) → dispatch (not a block)", func(t *testing.T) {
		// A decision that is a parent-child child of the task (how gb decision
		// create writes the --requesting-task backlink's inverse) does NOT block
		// via that edge. Only forward blocking-typed deps count — mirroring
		// gb ready / kd list --no-blockers. The decision blocks via a separate
		// `blocks` edge, which is absent here, so this task is dispatchable.
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "parent-child", "gd-dec")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-dec": {ID: "gd-dec", Type: "decision", Status: "open"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (parent-child is not a blocking edge), got false; reason=%q", got.Reason)
		}
	})

	t.Run("depends-on and blocked-by edges also count", func(t *testing.T) {
		for _, depType := range []string{"depends-on", "blocked-by"} {
			in := DispatchGateInput{
				TaskID: "gd-task",
				Deps:   []beadsapi.Dependency{gateDep("gd-task", depType, "gd-dec")},
				Blockers: map[string]*beadsapi.BeadDetail{
					"gd-dec": {ID: "gd-dec", Type: "decision", Status: "open"},
				},
			}
			got := EvaluateDispatchGate(in)
			if got.MayDispatch {
				t.Errorf("depType=%q: expected MayDispatch=false, got true", depType)
			}
		}
	})

	t.Run("unresolved blocker (missing from map) → dispatch (fail-open)", func(t *testing.T) {
		// A dep target we could not fetch cannot be proven an operator park, so
		// we keep dispatching rather than strand a possibly-actionable MR.
		in := DispatchGateInput{
			TaskID:   "gd-task",
			Deps:     []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-missing")},
			Blockers: map[string]*beadsapi.BeadDetail{},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (unresolved blocker, fail-open), got false; reason=%q", got.Reason)
		}
	})

	t.Run("duplicate blocker edges are reported once", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps: []beadsapi.Dependency{
				gateDep("gd-task", "blocks", "gd-dec"),
				gateDep("gd-task", "depends-on", "gd-dec"),
			},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-dec": {ID: "gd-dec", Type: "decision", Status: "open"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true")
		}
		if len(got.BlockerIDs) != 1 {
			t.Errorf("BlockerIDs = %v, want exactly one (deduped)", got.BlockerIDs)
		}
	})

	// --- REFERENCE signal (R1, bug ka-0000001050) -------------------------------
	// A decision/escalation that only NAMES the MR by URL/!iid (no blocks edge, or
	// no linked task at all) must also suppress a fresh crew. This is the durable
	// gap the old one-cycle bash DEDUP left open.

	const (
		mr6428URL = "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6428"
	)

	t.Run("live repro ka-0000004748 — open decision names !6428 with NO blocks edge → SUPPRESS", func(t *testing.T) {
		// The exact durable-gate gap: decision ka-0000004748 was filed by a
		// different agent, references the MR only by "!6428" in its title, and has
		// no blocks edge to the walked task. The blocking-edge scan finds nothing;
		// the reference scan catches it.
		in := DispatchGateInput{
			TaskID: "ka-0000006102",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "ka-0000004748", Type: "decision", Status: "open",
					Title: "Merge MR !6428 (terraform: KMS envelope encryption for EKS Secrets on prod-gardenio-eks)?"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (open decision references !6428), got true; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 1 || got.BlockerIDs[0] != "ka-0000004748" {
			t.Errorf("BlockerIDs = %v, want [ka-0000004748]", got.BlockerIDs)
		}
	})

	t.Run("orphan MR (no task) referenced by an open decision → SUPPRESS", func(t *testing.T) {
		// No linked task, so the whole blocking-edge path is skipped; the reference
		// signal alone must still dedup.
		in := DispatchGateInput{
			MRURL: mr6428URL,
			MRIID: 6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-dec", Type: "decision", Status: "open",
					Description: "Please merge " + mr6428URL + " once approved."},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (orphan MR referenced by open decision), got true; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 1 || got.BlockerIDs[0] != "gd-dec" {
			t.Errorf("BlockerIDs = %v, want [gd-dec]", got.BlockerIDs)
		}
	})

	t.Run("open escalation referencing the MR by full URL → SUPPRESS", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-esc", Type: "escalation", Status: "open",
					Description: "Approve + merge " + mr6428URL},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (escalation references URL), got true; reason=%q", got.Reason)
		}
	})

	t.Run("self-exec gitlab_mr_merge escalation referencing the MR → DISPATCH (bug ka-0000002517)", func(t *testing.T) {
		// The self-suppression deadlock: a despawning crew files a
		// gitlab_mr_merge escalation as its re-entry handle (init_prompts.go).
		// That is a merge the group bot can perform itself, not an operator hold,
		// so it must NOT suppress gardener-delivery's own merge — otherwise the
		// escalation (which only clears when the MR merges) deadlocks the gardener
		// forbidden from merging.
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-esc", Type: "escalation", Status: "open",
					Fields:      map[string]string{"operation_type": "gitlab_mr_merge"},
					Description: "Approve + merge " + mr6428URL},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (self-exec merge escalation is not a gate), got false; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 0 {
			t.Errorf("BlockerIDs = %v, want empty", got.BlockerIDs)
		}
	})

	t.Run("self-exec merge escalation on the BLOCKING edge, gardenio MR → DISPATCH (bug ka-0000002517)", func(t *testing.T) {
		// Same rule via the blocking-edge signal: the crew wired a `blocks` edge
		// to its own gitlab_mr_merge handle. On gardenio (self-mergeable by policy)
		// this is still self-executable, still not a gate. MRProjectPath supplies
		// the repo condition the carve-out now requires (bug ka-0000003768).
		in := DispatchGateInput{
			TaskID:        "gd-task",
			MRProjectPath: "Acme/Platform/gardenio",
			Deps:          []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-esc")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-esc": {ID: "gd-esc", Type: "escalation", Status: "open",
					Fields: map[string]string{"operation_type": "gitlab-mr-approve-merge"}},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (self-exec merge escalation, hyphen spelling, gardenio), got false; reason=%q", got.Reason)
		}
	})

	// --- REPO CONDITION on the self-exec carve-out (bug ka-0000003768) -----------
	// A gitlab_mr_merge escalation is self-executable — and therefore NOT a gate —
	// ONLY on a repo whose policy lets the bot self-merge (gardenio). On protected
	// `main` in every other repo the bot cannot self-approve, so the same
	// escalation IS a real operator gate and MUST suppress. Before this fix the
	// carve-out had no repo condition, so non-gardenio approve+merge escalations
	// never suppressed and gardener-delivery re-dispatched a fresh Opus crew every
	// cycle forever (80 crew gens / 88 HELD re-verifications measured).

	const (
		mrMonorepoURL       = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/1987"
		mrSiteDeploymentURL = "https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1234"
	)

	t.Run("self-exec gitlab_mr_merge escalation on a NON-gardenio (monorepo) MR → SUPPRESS (bug ka-0000003768)", func(t *testing.T) {
		// The exact leak: monorepo!1987 re-crewed 24 generations. The bot is
		// Developer/30 on protected monorepo main and cannot self-approve, so this
		// escalation is a real operator gate — it must suppress, not dispatch.
		in := DispatchGateInput{
			TaskID:        "gd-gi7rTrRM1V",
			MRURL:         mrMonorepoURL,
			MRProjectPath: "Acme/Platform/monorepo",
			MRIID:         1987,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-sP3vPQ3OxZ", Type: "escalation", Status: "open",
					Fields:      map[string]string{"operation_type": "gitlab-mr-approve-merge"},
					Description: "Approve + merge " + mrMonorepoURL},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (monorepo is not self-mergeable; escalation is a real gate), got true; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 1 || got.BlockerIDs[0] != "gd-sP3vPQ3OxZ" {
			t.Errorf("BlockerIDs = %v, want [gd-sP3vPQ3OxZ]", got.BlockerIDs)
		}
	})

	t.Run("self-exec merge escalation on the BLOCKING edge, non-gardenio MR → SUPPRESS (bug ka-0000003768)", func(t *testing.T) {
		// Same repo condition via the blocking-edge signal: site-deployment!1234
		// re-crewed 32 generations. Protected non-gardenio main → real gate.
		in := DispatchGateInput{
			TaskID:        "gd-O4PBGxxJGI",
			MRURL:         mrSiteDeploymentURL,
			MRProjectPath: "Acme/Platform/site-deployment",
			MRIID:         1234,
			Deps:          []beadsapi.Dependency{gateDep("gd-O4PBGxxJGI", "blocks", "gd-wYmzQmaTpG")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-wYmzQmaTpG": {ID: "gd-wYmzQmaTpG", Type: "escalation", Status: "open",
					Fields: map[string]string{"operation_type": "gitlab_mr_merge"}},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (site-deployment is not self-mergeable), got true; reason=%q", got.Reason)
		}
		if len(got.BlockerIDs) != 1 || got.BlockerIDs[0] != "gd-wYmzQmaTpG" {
			t.Errorf("BlockerIDs = %v, want [gd-wYmzQmaTpG]", got.BlockerIDs)
		}
	})

	t.Run("gate MR repo unresolved, escalation title names a non-gardenio repo → SUPPRESS (bug ka-0000003768)", func(t *testing.T) {
		// When the gate could not resolve the MR's own project path, the carve-out
		// falls back to the repo the escalation itself names ("acme-helm-chart !453").
		// Non-gardenio → still a gate.
		in := DispatchGateInput{
			TaskID: "gd-nwAhsYkQ9j",
			Deps:   []beadsapi.Dependency{gateDep("gd-nwAhsYkQ9j", "blocks", "gd-bng3c4wWKu")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-bng3c4wWKu": {ID: "gd-bng3c4wWKu", Type: "escalation", Status: "open",
					Title:  "Approve+merge acme-helm-chart !453 — env=e2e externalLabel",
					Fields: map[string]string{"operation_type": "gitlab_mr_merge"}},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (acme-helm-chart is not self-mergeable), got true; reason=%q", got.Reason)
		}
	})

	t.Run("gate MR repo unresolved AND escalation names no repo → SUPPRESS (fail toward gate)", func(t *testing.T) {
		// No repo signal anywhere: the carve-out is an exception that must earn a
		// positively-confirmed self-mergeable repo, so an unprovable one falls back
		// to treating the escalation as a real gate (bounded suppression + R3 nudge
		// beats unbounded dispatch).
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-esc")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-esc": {ID: "gd-esc", Type: "escalation", Status: "open",
					Fields: map[string]string{"operation_type": "gitlab_mr_merge"}},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (repo unconfirmed → treat as gate), got true; reason=%q", got.Reason)
		}
	})

	t.Run("gate MR repo unresolved, escalation's own mr_url names gardenio → DISPATCH", func(t *testing.T) {
		// The fallback resolves gardenio from the escalation's own mr_url field, so
		// the carve-out still fires and the original deadlock (ka-0000002517) stays
		// fixed even when the gate could not resolve the MR path itself.
		in := DispatchGateInput{
			TaskID: "gd-task",
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-esc")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-esc": {ID: "gd-esc", Type: "escalation", Status: "open",
					Fields: map[string]string{
						"operation_type": "gitlab_mr_merge",
						"mr_url":         mr6428URL,
					}},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (escalation mr_url names gardenio), got false; reason=%q", got.Reason)
		}
	})

	t.Run("NON-self-exec escalation (aws_iam) referencing the MR → still SUPPRESS", func(t *testing.T) {
		// A genuine operator action the bot cannot perform stays a gate.
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-esc", Type: "escalation", Status: "open",
					Fields:      map[string]string{"operation_type": "aws_iam"},
					Description: "Grant IAM to land " + mr6428URL},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (aws_iam is a real operator gate), got true; reason=%q", got.Reason)
		}
	})

	t.Run("decision carrying a merge operation_type still SUPPRESSES (only escalations self-exec)", func(t *testing.T) {
		// operation_type belongs to escalations; a decision is always an operator
		// fork. A stray field on a decision must not defeat the gate.
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-dec", Type: "decision", Status: "open",
					Fields:      map[string]string{"operation_type": "gitlab_mr_merge"},
					Description: "Merge/hold fork for " + mr6428URL},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (decision is an operator fork regardless of fields), got true; reason=%q", got.Reason)
		}
	})

	t.Run("open decision mentions the linked task bead-id (mentions-index signal) → SUPPRESS", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "ka-0000006102",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-dec", Type: "decision", Status: "open",
					Description: "Merge-gate for task ka-0000006102 — waiting on operator."},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (decision mentions the task id), got true; reason=%q", got.Reason)
		}
	})

	t.Run("reference candidate for a DIFFERENT MR iid → dispatch (no false suppress)", func(t *testing.T) {
		// A decision that names !6400, walked against !6428, must NOT suppress —
		// !6428 as a substring guard: 6400 != 6428.
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-dec", Type: "decision", Status: "open", Title: "Merge MR !6400 (unrelated)"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (decision is about !6400, not !6428), got false; reason=%q", got.Reason)
		}
	})

	t.Run("CLOSED reference candidate → dispatch (stale decision no longer parks)", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-dec", Type: "decision", Status: "closed", Title: "Merge MR !6428 (resolved)"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (closed decision), got false; reason=%q", got.Reason)
		}
	})

	t.Run("reference candidate of a NON-gate type → dispatch (a task/mr bead is not an operator park)", func(t *testing.T) {
		// gatherDispatchGateRefCandidates only queries decision/escalation, but the
		// evaluator re-checks type so a mis-supplied candidate can never suppress.
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-mr", Type: "merge-request", Status: "open", Title: "MR !6428"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (merge-request bead is not a gate), got false; reason=%q", got.Reason)
		}
	})

	t.Run("bare !iid match without a project qualifier → SUPPRESS", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRIID:  6428, // no URL resolved, only the iid
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-dec", Type: "decision", Status: "open", Title: "hold !6428 for review"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (bare !6428 matches), got true; reason=%q", got.Reason)
		}
	})

	t.Run("blocks-edge AND reference handles merge, deduped and sorted", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			Deps:   []beadsapi.Dependency{gateDep("gd-task", "blocks", "gd-aaa")},
			Blockers: map[string]*beadsapi.BeadDetail{
				"gd-aaa": {ID: "gd-aaa", Type: "escalation", Status: "open"},
			},
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-zzz", Type: "decision", Status: "open", Title: "Merge MR !6428"},
				// same handle already found via the edge — must not double-count.
				{ID: "gd-aaa", Type: "escalation", Status: "open", Title: "Merge MR !6428"},
			},
		}
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true; reason=%q", got.Reason)
		}
		want := []string{"gd-aaa", "gd-zzz"}
		if len(got.BlockerIDs) != len(want) {
			t.Fatalf("BlockerIDs = %v, want %v (deduped+sorted)", got.BlockerIDs, want)
		}
		for i := range want {
			if got.BlockerIDs[i] != want[i] {
				t.Fatalf("BlockerIDs = %v, want %v", got.BlockerIDs, want)
			}
		}
	})

	t.Run("no reference and no block → dispatch (reason names both signals)", func(t *testing.T) {
		in := DispatchGateInput{
			TaskID: "gd-task",
			MRURL:  mr6428URL,
			MRIID:  6428,
			RefCandidates: []*beadsapi.BeadDetail{
				{ID: "gd-other", Type: "decision", Status: "open", Title: "Merge MR !9999 (unrelated)"},
			},
		}
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true, got false; reason=%q", got.Reason)
		}
	})
}

func TestBeadReferencesMR(t *testing.T) {
	const (
		url  = "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6428"
		proj = "Acme/Platform/gardenio"
	)
	cases := []struct {
		name   string
		bead   *beadsapi.BeadDetail
		mrURL  string
		mrProj string
		mrIID  int
		taskID string
		want   bool
	}{
		{"nil bead", nil, url, proj, 6428, "", false},
		{"exact url in title", &beadsapi.BeadDetail{Title: "merge " + url}, url, proj, 6428, "", true},
		{"exact url in description", &beadsapi.BeadDetail{Description: "please merge " + url + " today"}, url, proj, 6428, "", true},
		{"bang ref full-path qualified", &beadsapi.BeadDetail{Title: "Acme/Platform/gardenio!6428 green"}, "", proj, 6428, "", true},
		{"bang ref repo-segment qualified", &beadsapi.BeadDetail{Title: "gardenio!6428 green"}, "", proj, 6428, "", true},
		{"bare bang ref", &beadsapi.BeadDetail{Title: "hold !6428"}, "", proj, 6428, "", true},
		{"second bang ref in a list", &beadsapi.BeadDetail{Description: "supersedes !6400, merge !6428 now"}, "", proj, 6428, "", true},
		{"wrong iid", &beadsapi.BeadDetail{Title: "Merge MR !6400"}, url, proj, 6428, "", false},
		{"iid substring is not a match", &beadsapi.BeadDetail{Title: "Merge MR !64280"}, "", proj, 6428, "", false},
		{"different full url same iid tail", &beadsapi.BeadDetail{Title: "https://gitlab.com/x/y/-/merge_requests/16428"}, url, proj, 0, "", false},
		// Cross-project IID collision: a DIFFERENT repo's !6428 must NOT match.
		{"cross-project qualified bang ref → no match", &beadsapi.BeadDetail{Title: "Merge monorepo!6428"}, "", proj, 6428, "", false},
		{"cross-project full-path bang ref → no match", &beadsapi.BeadDetail{Title: "Acme/Platform/monorepo!6428"}, "", proj, 6428, "", false},
		// A qualified ref with an unresolved MR project cannot be confirmed → no match.
		{"qualified bang ref but MR project unknown → no match", &beadsapi.BeadDetail{Title: "monorepo!6428"}, "", "", 6428, "", false},
		// A bare ref still matches even when the MR project is unknown.
		{"bare bang ref, MR project unknown → match", &beadsapi.BeadDetail{Title: "hold !6428"}, "", "", 6428, "", true},
		{"task-id mention", &beadsapi.BeadDetail{Description: "gate for ka-0000006102"}, "", "", 0, "ka-0000006102", true},
		{"task-id substring is not a match", &beadsapi.BeadDetail{Description: "gd-8gLZPI7Sh0EXTRA"}, "", "", 0, "ka-0000006102", false},
		{"no signal at all", &beadsapi.BeadDetail{Title: "unrelated decision"}, url, proj, 6428, "gd-task", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := beadReferencesMR(tc.bead, tc.mrURL, tc.mrProj, tc.mrIID, tc.taskID); got != tc.want {
				t.Errorf("beadReferencesMR(%+v, %q, %q, %d, %q) = %v, want %v", tc.bead, tc.mrURL, tc.mrProj, tc.mrIID, tc.taskID, got, tc.want)
			}
		})
	}
}

func TestBangRefProjectMatches(t *testing.T) {
	const mrp = "Acme/Platform/gardenio"
	cases := []struct {
		bangProj string
		mrProj   string
		want     bool
	}{
		{"", mrp, true},                        // bare ref always matches on iid
		{"", "", true},                         // bare ref, unknown MR project
		{"gardenio", mrp, true},                // repo-segment
		{"Acme/Platform/gardenio", mrp, true},  // full path
		{"Platform/gardenio", mrp, true},       // path suffix
		{"GARDENIO", mrp, true},                // case-insensitive
		{"monorepo", mrp, false},               // different repo
		{"Acme/Platform/monorepo", mrp, false}, // different full path
		{"gardenio", "", false},                // qualified but MR project unknown
		{"gardenio-extra", mrp, false},         // near-miss segment
	}
	for _, tc := range cases {
		t.Run(tc.bangProj+"|"+tc.mrProj, func(t *testing.T) {
			if got := bangRefProjectMatches(tc.bangProj, tc.mrProj); got != tc.want {
				t.Errorf("bangRefProjectMatches(%q, %q) = %v, want %v", tc.bangProj, tc.mrProj, got, tc.want)
			}
		})
	}
}

// TestEvaluateDispatchGateAgingNudge covers R3 (bug ka-0000007496): a
// persistently-parked MR earns exactly ONE aging nudge once the park ages past
// DispatchGateNudgeAge, and never a second. Every case asserts the suppression
// itself is unchanged — the nudge rides alongside a still-false MayDispatch.
func TestEvaluateDispatchGateAgingNudge(t *testing.T) {
	// A fixed reference clock; parking handles are dated relative to it.
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	// A handle filed `age` before now, of the given type, optionally already
	// nudged (marker set).
	handle := func(id, typ string, age time.Duration, nudged bool) *beadsapi.BeadDetail {
		b := &beadsapi.BeadDetail{
			ID: id, Type: typ, Status: "open",
			CreatedAt: now.Add(-age),
			Fields:    map[string]string{},
		}
		if nudged {
			b.Fields[DispatchGateNudgedAtField] = "2026-08-04T09:00:00Z"
		}
		return b
	}
	// Build a suppression input from a reference candidate (no linked task), so
	// the nudge path is exercised through the same handle plumbing the gate uses
	// for an orphan MR. mrIID names the candidate so it references the MR.
	suppressInput := func(cands []*beadsapi.BeadDetail, now time.Time) DispatchGateInput {
		return DispatchGateInput{
			MRURL:         "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6428",
			MRProjectPath: "Acme/Platform/gardenio",
			MRIID:         6428,
			RefCandidates: cands,
			Now:           now,
		}
	}
	// Give each candidate text that references MR !6428 so beadReferencesMR fires.
	ref := func(b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
		b.Title = "Merge MR !6428 (KMS prod encryption)?"
		return b
	}

	t.Run("parked past threshold, never nudged → SUPPRESS + Nudge", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("ka-0000004748", "decision", 3*time.Hour, false)),
		}, now)
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true; reason=%q", got.Reason)
		}
		if got.Nudge == nil {
			t.Fatalf("expected a Nudge for a 3h-old park, got nil")
		}
		if got.Nudge.HandleID != "ka-0000004748" {
			t.Errorf("Nudge.HandleID = %q, want ka-0000004748", got.Nudge.HandleID)
		}
		if got.Nudge.HandleType != "decision" {
			t.Errorf("Nudge.HandleType = %q, want decision", got.Nudge.HandleType)
		}
		if got.Nudge.GateAge != 3*time.Hour {
			t.Errorf("Nudge.GateAge = %v, want 3h", got.Nudge.GateAge)
		}
	})

	t.Run("parked but younger than threshold → SUPPRESS, no Nudge", func(t *testing.T) {
		// 1h old — under the 2h (6-cycle) threshold.
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", 1*time.Hour, false)),
		}, now)
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true")
		}
		if got.Nudge != nil {
			t.Errorf("expected no Nudge for a 1h-old park, got %+v", got.Nudge)
		}
	})

	t.Run("exactly at threshold → Nudge fires (boundary is inclusive)", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", DispatchGateNudgeAge, false)),
		}, now)
		got := EvaluateDispatchGate(in)
		if got.Nudge == nil {
			t.Fatalf("expected a Nudge exactly at the threshold, got nil")
		}
	})

	t.Run("one cycle under threshold → no Nudge", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", DispatchGateNudgeAge-time.Second, false)),
		}, now)
		got := EvaluateDispatchGate(in)
		if got.Nudge != nil {
			t.Errorf("expected no Nudge just under the threshold, got %+v", got.Nudge)
		}
	})

	t.Run("already nudged (marker set) → SUPPRESS, no second Nudge (one-shot)", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", 3*time.Hour, true)),
		}, now)
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true")
		}
		if got.Nudge != nil {
			t.Errorf("expected no second Nudge once marker is set, got %+v", got.Nudge)
		}
	})

	t.Run("zero Now disables the nudge but not the suppression (fail-closed on nudge)", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", 3*time.Hour, false)),
		}, time.Time{})
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false even with zero Now, got true")
		}
		if got.Nudge != nil {
			t.Errorf("expected no Nudge with a zero Now, got %+v", got.Nudge)
		}
	})

	t.Run("handle with unknown filing time → no Nudge (cannot prove age)", func(t *testing.T) {
		h := ref(handle("gd-dec", "decision", 0, false))
		h.CreatedAt = time.Time{} // unknown
		in := suppressInput([]*beadsapi.BeadDetail{h}, now)
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true")
		}
		if got.Nudge != nil {
			t.Errorf("expected no Nudge when filing time is unknown, got %+v", got.Nudge)
		}
	})

	t.Run("dispatch path (not suppressed) → never a Nudge", func(t *testing.T) {
		// A candidate that does NOT reference the MR — the gate dispatches, and a
		// dispatch must never carry a nudge.
		h := handle("gd-dec", "decision", 5*time.Hour, false)
		h.Title = "Unrelated decision about MR !9999"
		in := suppressInput([]*beadsapi.BeadDetail{h}, now)
		got := EvaluateDispatchGate(in)
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (no reference), got false; reason=%q", got.Reason)
		}
		if got.Nudge != nil {
			t.Errorf("expected no Nudge on the dispatch path, got %+v", got.Nudge)
		}
	})

	t.Run("two handles park the MR → Nudge reports the OLDEST park's age", func(t *testing.T) {
		// gd-aaa filed 5h ago, gd-bbb 3h ago. The park began 5h ago, so the nudge
		// must report gd-aaa / 5h even though gd-bbb is lex-earlier here it is not.
		older := ref(handle("gd-bbb-newer", "decision", 3*time.Hour, false))
		oldest := ref(handle("gd-aaa-older", "escalation", 5*time.Hour, false))
		in := suppressInput([]*beadsapi.BeadDetail{older, oldest}, now)
		got := EvaluateDispatchGate(in)
		if got.Nudge == nil {
			t.Fatalf("expected a Nudge, got nil")
		}
		if got.Nudge.HandleID != "gd-aaa-older" {
			t.Errorf("Nudge.HandleID = %q, want gd-aaa-older (oldest park)", got.Nudge.HandleID)
		}
		if got.Nudge.GateAge != 5*time.Hour {
			t.Errorf("Nudge.GateAge = %v, want 5h", got.Nudge.GateAge)
		}
	})

	t.Run("oldest handle ALREADY nudged, newer not → no Nudge (one-shot keys on the reported handle)", func(t *testing.T) {
		// The oldest park (gd-aaa-older) already carries the marker, so the whole
		// park is considered nudged — we do not fall through to re-nudge on a
		// newer sibling handle.
		older := ref(handle("gd-bbb-newer", "decision", 3*time.Hour, false))
		oldest := ref(handle("gd-aaa-older", "escalation", 5*time.Hour, true))
		in := suppressInput([]*beadsapi.BeadDetail{older, oldest}, now)
		got := EvaluateDispatchGate(in)
		if got.Nudge != nil {
			t.Errorf("expected no Nudge once the oldest park is marked, got %+v", got.Nudge)
		}
	})

	// NudgeAge is the injection seam the dispatch-gate e2e drives (via
	// GB_DISPATCH_GATE_NUDGE_AGE) so a freshly-created handle — whose created_at
	// cannot be backdated on the shared daemon — can cross the threshold. A
	// non-positive value must keep the compiled ~2h default; a small positive one
	// must let a young park nudge.
	t.Run("NudgeAge override: a 1m park nudges when the threshold is lowered to 30s", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", 1*time.Minute, false)),
		}, now)
		in.NudgeAge = 30 * time.Second
		got := EvaluateDispatchGate(in)
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false, got true")
		}
		if got.Nudge == nil {
			t.Fatalf("expected a Nudge for a 1m park under a 30s threshold, got nil")
		}
		if got.Nudge.GateAge != 1*time.Minute {
			t.Errorf("Nudge.GateAge = %v, want 1m (the real park age, not the threshold)", got.Nudge.GateAge)
		}
	})

	t.Run("NudgeAge override: still respects one-shot marker (no second nudge)", func(t *testing.T) {
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", 1*time.Minute, true)),
		}, now)
		in.NudgeAge = 30 * time.Second
		got := EvaluateDispatchGate(in)
		if got.Nudge != nil {
			t.Errorf("expected no second Nudge once marker is set even under a lowered threshold, got %+v", got.Nudge)
		}
	})

	t.Run("non-positive NudgeAge falls back to the compiled ~2h default", func(t *testing.T) {
		// A 1m park with NudgeAge unset (0) must NOT nudge — the default governs.
		in := suppressInput([]*beadsapi.BeadDetail{
			ref(handle("gd-dec", "decision", 1*time.Minute, false)),
		}, now)
		in.NudgeAge = 0
		got := EvaluateDispatchGate(in)
		if got.Nudge != nil {
			t.Errorf("expected no Nudge for a 1m park at the default ~2h threshold, got %+v", got.Nudge)
		}
	})
}
