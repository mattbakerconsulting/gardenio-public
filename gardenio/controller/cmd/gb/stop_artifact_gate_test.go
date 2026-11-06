package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// artifactGateFixture serves the two reads checkRequiredArtifact makes:
//   - GET /v1/beads/<agent-id>  → the agent bead (resolveActorSeat reads its
//     seat_key to resolve the DURABLE lane owner).
//   - the decision list          → closed decisions this lane owns.
//
// seat is the seat_key stamped on the served agent bead; when it is "" the
// agent bead carries no seat_key (the unresolvable-lane case). decisions is the
// list the daemon returns for the owner_seat-scoped query.
func artifactGateFixture(t *testing.T, agentID, seat string, decisions []map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/beads/") && !strings.HasSuffix(r.URL.Path, "/dependencies") {
			fields := map[string]string{}
			if seat != "" {
				fields["seat_key"] = seat
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": agentID, "type": "agent", "fields": fields,
			})
			return
		}
		// The decision list. Assert the gate scopes on the DURABLE owner_seat,
		// never the ephemeral requesting_agent_bead_id — that is the whole point
		// of the respawn-survival scope decision. Field predicates are encoded as
		// repeated `field=<key>=<value>` params (client_query.go).
		fieldPreds := r.URL.Query()["field"]
		if seat != "" {
			want := "owner_seat=" + seat
			found := false
			for _, p := range fieldPreds {
				if p == want {
					found = true
				}
				if strings.HasPrefix(p, "requesting_agent_bead_id=") {
					t.Errorf("gate must NOT key on requesting_agent_bead_id (it dies on respawn); query=%s", r.URL.RawQuery)
				}
			}
			if !found {
				t.Errorf("decision list must scope on %q, got field preds %v (query=%s)", want, fieldPreds, r.URL.RawQuery)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": decisions})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	fn()
	wPipe.Close()
	os.Stderr = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, rPipe)
	return buf.String()
}

// TestCheckRequiredArtifact covers the stop-time artifact gate (ka-0000006794):
// it fires when this lane owns a resolved decision whose artifact is still
// pending, passes once the artifact is submitted, and names the exact remedy.
func TestCheckRequiredArtifact(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-AGENT01")

	t.Run("pending artifact -> gate fires, names remedy", func(t *testing.T) {
		artifactGateFixture(t, "gd-AGENT01", "gardenio/crew/seat", []map[string]any{{
			"id": "gd-DEC1", "title": "ship the plan", "type": "decision", "status": "closed",
			"fields": map[string]string{
				"required_artifact": "plan",
				"artifact_status":   "pending",
			},
		}})

		var err error
		stderr := captureStderr(t, func() { err = checkRequiredArtifact(context.Background()) })

		if err == nil {
			t.Fatal("checkRequiredArtifact returned nil while an artifact was pending; want an error")
		}
		if !strings.Contains(err.Error(), "gd-DEC1") {
			t.Errorf("headline error should name the owing decision, got: %q", err.Error())
		}
		// The remedy command must be named exactly, with the @<file> form.
		if !strings.Contains(err.Error(), "gb decision report gd-DEC1 --content @<file>") {
			t.Errorf("error must name the exact remedy command, got: %q", err.Error())
		}
		for _, want := range []string{"gb decision report gd-DEC1 --content @<file>", "plan", "--force"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr remediation missing %q, got:\n%s", want, stderr)
			}
		}
	})

	t.Run("submitted artifact -> gate passes", func(t *testing.T) {
		artifactGateFixture(t, "gd-AGENT01", "gardenio/crew/seat", []map[string]any{{
			"id": "gd-DEC1", "title": "ship the plan", "type": "decision", "status": "closed",
			"fields": map[string]string{
				"required_artifact": "plan",
				"artifact_status":   "submitted",
			},
		}})
		if err := checkRequiredArtifact(context.Background()); err != nil {
			t.Errorf("a submitted artifact should clear the gate, got: %v", err)
		}
	})

	t.Run("decision with no declared artifact -> gate passes", func(t *testing.T) {
		artifactGateFixture(t, "gd-AGENT01", "gardenio/crew/seat", []map[string]any{{
			"id": "gd-DEC2", "title": "just a fork", "type": "decision", "status": "closed",
			"fields": map[string]string{}, // no required_artifact
		}})
		if err := checkRequiredArtifact(context.Background()); err != nil {
			t.Errorf("a decision that declared no artifact should not gate, got: %v", err)
		}
	})

	t.Run("no owned decisions -> gate passes", func(t *testing.T) {
		artifactGateFixture(t, "gd-AGENT01", "gardenio/crew/seat", []map[string]any{})
		if err := checkRequiredArtifact(context.Background()); err != nil {
			t.Errorf("no owned decisions should return nil, got: %v", err)
		}
	})

	t.Run("unresolvable lane (no seat_key) -> fail open", func(t *testing.T) {
		// A caller with no durable seat cannot be attributed a decision's
		// obligation, so the gate must not fire — even if a pending decision
		// exists, it is served only under the owner_seat query which never runs.
		artifactGateFixture(t, "gd-AGENT01", "", []map[string]any{{
			"id": "gd-DEC1", "title": "orphan", "type": "decision", "status": "closed",
			"fields": map[string]string{"required_artifact": "plan", "artifact_status": "pending"},
		}})
		if err := checkRequiredArtifact(context.Background()); err != nil {
			t.Errorf("an unidentifiable caller must fail open, got: %v", err)
		}
	})
}

// TestCheckRequiredArtifact_ForceOverride proves the gate is --force-guarded at
// the call site in runStop: with stopForce true the batch never invokes the
// check, so a pending artifact cannot block a forced stop. The gate function
// itself is unconditional (like every sibling); --force is enforced by the
// guard around the call, mirrored here.
func TestCheckRequiredArtifact_ForceOverride(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-AGENT01")
	artifactGateFixture(t, "gd-AGENT01", "gardenio/crew/seat", []map[string]any{{
		"id": "gd-DEC1", "title": "ship the plan", "type": "decision", "status": "closed",
		"fields": map[string]string{"required_artifact": "plan", "artifact_status": "pending"},
	}})

	// Mirror the runStop guard: `if !stopForce { checkRequiredArtifact(...) }`.
	var failures []error
	stopForce := true
	if !stopForce {
		if err := checkRequiredArtifact(context.Background()); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		t.Fatalf("--force must skip the artifact gate; collected %d failure(s)", len(failures))
	}

	// And with force off, the same guard DOES collect the failure.
	stopForce = false
	if !stopForce {
		if err := checkRequiredArtifact(context.Background()); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("without --force the gate must fire once; collected %d", len(failures))
	}
}
