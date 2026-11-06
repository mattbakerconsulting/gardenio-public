package gardenhub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// escYardWorld adds to the card world one open escalation of each family to gardenio, the four states among them, and 90
// merges to a project "zoo", past what its cell holds at its widest (88 slots, 29.10, 29.11), so it ends in a "+N" crate. The Escalations cell of each draws them
// (homestead-world-design.md 28.11, 28.13).
func escYardWorld(src *fakeFarmSrc, now time.Time) {
	at := now.Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	for i, e := range []struct{ id, op, state string }{
		{"gd-Emerge", "github_pr_merge", "pending"},
		{"gd-Edeploy", "helm_deploy", "approved"},
		{"gd-Econfig", "helm_chart_edit", "pending"},
		{"gd-Eprompt", "trigger_prompt_install", "rejected"},
		{"gd-Ebeads", "kbeads_admin_write", "failed"},
		{"gd-Eaccess", "k8s_rbac", "pending"},
		{"gd-Einfra", "terraform_apply", "approved"},
		{"gd-Eread", "cluster_read", "pending"},
		{"gd-Eother", "manual_operator_action", "rejected"},
	} {
		b := fb(e.id, "escalation", "open", "", int64(800+i))
		b.Title = "Escalation " + e.op
		b.Fields["operation_type"] = e.op
		switch e.state {
		case "approved":
			b.Fields["approved_at"] = at
		case "rejected":
			b.Fields["result_status"], b.Fields["result"] = "failure", `{"error":"rejected"}`
		case "failed":
			b.Fields["result_status"] = "failure"
		}
		src.add(b)
	}
	for i := 0; i < 90; i++ {
		b := fb(fmt.Sprintf("gd-Ezoo%02d", i), "escalation", "open", "", int64(820+i))
		b.Labels, b.Fields["operation_type"] = []string{"project:zoo"}, "github_pr_merge"
		src.add(b)
	}
}

// TestEscalationYardUnderChromium walks the Escalations cell in a real browser (step 6 gd-4FIF5pzC6O; testdata/
// escyard_browser.js): each escalation is its own cell in ESCKIND's order, named in ESCKIND's words; hover and focus show its
// sign tip, with Approve and Reject on a pending one for a viewer who may approve; Enter and Space open the bead view; past
// the cap a "+N" crate counts the rest; the counts ride the Home sign and the project's words at the World level; under reduced motion no gear turns.
// An approve answered in place posts to the viewer's /b/{id}/escalation/approve, which here closes the bead, and the next
// farm document takes it out of the cell.
func TestEscalationYardUnderChromium(t *testing.T) {
	underChromium(t, "escyard_browser.js", func() *httptest.Server {
		now := time.Now()
		srv, rig := cardWorldRig(t, time.Now, "oidc:matt@example.com", nil, func(src *fakeFarmSrc) { escYardWorld(src, now) }, false)
		rig.page.CanApproveEscalation = func(*http.Request) bool { return true }
		rig.mux.HandleFunc("POST /b/{id}/escalation/{act}", func(w http.ResponseWriter, r *http.Request) {
			rig.src.mutate(r.PathValue("id"), func(b *beadsapi.BeadDetail) { b.Status = "closed" })
			rig.farm.Invalidate()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ok":true,"bead":%q}`, r.PathValue("id"))
		})
		return srv
	}, os.Getenv("GARDENHUB_ESCYARD_SHOTS"))
}

// TestEscalationExecutorUnderChromium (step 7 gd-B5aDmNhmjz; testdata/escexec_browser.js): the executor of gd-Eaccess, with no
// task and its escalation on no plan, is at the barn and also stands beside its icon in the Escalations cell (WORLD.escStands),
// facing it; hover shows its name and "Running access escalation: <title>", and its link opens its bead.
func TestEscalationExecutorUnderChromium(t *testing.T) {
	underChromium(t, "escexec_browser.js", func() *httptest.Server {
		now := time.Now()
		srv, _ := cardWorldRig(t, time.Now, "oidc:matt@example.com", nil, func(src *fakeFarmSrc) {
			escYardWorld(src, now)
			ex := agentBead("gd-gEscExec", "escalation-gd-eaccess", "crew", "working", "", 7990)
			ex.Fields["escalation_id"] = "gd-Eaccess"
			src.add(ex)
		}, false)
		return srv
	}, os.Getenv("GARDENHUB_ESCYARD_SHOTS"))
}
