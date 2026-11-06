package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Open-PR close guard tests (gd-lIKFejVISw). The fake GitHub lookup answers
// from a URL → state map and records what it was asked.

const testPRURL = "https://github.com/acme/app/pull/1525"

type fakeGitHubPRs struct {
	states map[string]string
	err    error
	asked  []string
}

func (f *fakeGitHubPRs) lookup(_ context.Context, prURL string) (string, error) {
	f.asked = append(f.asked, prURL)
	if f.err != nil {
		return "", f.err
	}
	return f.states[prURL], nil
}

func newOpenPRGuardServer(prState string) (*BeadsServer, *mockStore, http.Handler, *fakeGitHubPRs) {
	s, ms, h := newTestServer()
	gh := &fakeGitHubPRs{states: map[string]string{testPRURL: prState}}
	s.SetPRStateLookup(gh.lookup)
	return s, ms, h, gh
}

func seedPRTask(ms *mockStore, id string, typ model.BeadType) {
	seedBead(ms, id, typ, model.StatusInProgress, nil, map[string]any{"mr_url": testPRURL})
}

func TestCloseBead_OpenPR_Refused(t *testing.T) {
	for _, typ := range []model.BeadType{model.TypeTask, model.TypeBug} {
		t.Run(string(typ), func(t *testing.T) {
			_, ms, h, gh := newOpenPRGuardServer("open")
			seedPRTask(ms, "gd-task", typ)

			rec := doJSON(t, h, "POST", "/v1/beads/gd-task/close", nil)
			requireStatus(t, rec, http.StatusConflict)
			body := rec.Body.String()
			for _, want := range []string{
				"refusing to close gd-task", testPRURL, "still open",
				"gitlab_mr_merge", "--requesting-task gd-task --close-on-execute",
				"gh pr close " + testPRURL + " --comment", "kd close --force",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("refusal %q does not mention %q", body, want)
				}
			}
			if got := ms.beads["gd-task"].Status; got != model.StatusInProgress {
				t.Errorf("status = %q, want the task left in_progress", got)
			}
			if len(gh.asked) != 1 || gh.asked[0] != testPRURL {
				t.Errorf("GitHub asked %v, want [%s]", gh.asked, testPRURL)
			}
		})
	}
}

func TestCloseBead_MergedOrClosedPR_Closes(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			_, ms, h, _ := newOpenPRGuardServer(state)
			seedPRTask(ms, "gd-task", model.TypeTask)

			rec := doJSON(t, h, "POST", "/v1/beads/gd-task/close", nil)
			requireStatus(t, rec, http.StatusOK)
			if got := ms.beads["gd-task"].Status; got != model.StatusClosed {
				t.Errorf("status = %q, want closed", got)
			}
		})
	}
}

// A failed lookup is not evidence the PR is open: the close goes through.
func TestCloseBead_PRLookupError_Closes(t *testing.T) {
	_, ms, h, gh := newOpenPRGuardServer("open")
	gh.err = errors.New("github: 502")
	seedPRTask(ms, "gd-task", model.TypeTask)

	requireStatus(t, doJSON(t, h, "POST", "/v1/beads/gd-task/close", nil), http.StatusOK)
}

func TestCloseBead_OpenPRGuard_Scope(t *testing.T) {
	_, ms, h, gh := newOpenPRGuardServer("open")
	seedBead(ms, "gd-nopr", model.TypeTask, model.StatusOpen, nil, nil)
	seedPRTask(ms, "gd-epic", model.TypeEpic) // epics drain via auto_close.go
	seedPRTask(ms, "gd-feat", model.TypeFeature)

	for _, id := range []string{"gd-nopr", "gd-epic", "gd-feat"} {
		requireStatus(t, doJSON(t, h, "POST", "/v1/beads/"+id+"/close", nil), http.StatusOK)
	}
	if len(gh.asked) != 0 {
		t.Errorf("GitHub asked %v for beads outside the guard", gh.asked)
	}
}

func TestCloseBead_OpenPR_OperatorForce(t *testing.T) {
	_, ms, h, _ := newOpenPRGuardServer("open")
	seedPRTask(ms, "gd-task", model.TypeTask)

	req := httptest.NewRequest("POST", "/v1/beads/gd-task/close", nil)
	req.Header.Set(forceCloseHeader, "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // newTestServer signs in as an OIDC admin
	requireStatus(t, rec, http.StatusOK)
	if got := ms.beads["gd-task"].Status; got != model.StatusClosed {
		t.Errorf("status = %q, want an operator's --force to close", got)
	}
}

// An agent token's X-Force-Close is ignored.
func TestCloseBead_OpenPR_AgentForceRefused(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	gh := &fakeGitHubPRs{states: map[string]string{testPRURL: "open"}}
	s.SetPRStateLookup(gh.lookup)
	h := s.NewHTTPHandler(nil, withTestToken(nil, "agent-secret"))
	seedPRTask(ms, "gd-task", model.TypeTask)

	req := httptest.NewRequest("POST", "/v1/beads/gd-task/close", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	req.Header.Set(forceCloseHeader, "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusConflict)
	if got := ms.beads["gd-task"].Status; got != model.StatusInProgress {
		t.Errorf("status = %q, want an agent's --force refused", got)
	}

	if callerMayForceCloseOverOpenPR(withAuthMethod(context.Background(), authMethodStatic)) {
		t.Error("a fleet token may force-close over an open PR")
	}
}

// `kd update --status closed` is a close too.
func TestUpdateBead_StatusClosed_OpenPR_Refused(t *testing.T) {
	_, ms, h, _ := newOpenPRGuardServer("open")
	seedPRTask(ms, "gd-task", model.TypeTask)

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-task", map[string]any{"status": "closed"})
	requireStatus(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), testPRURL) {
		t.Errorf("refusal %q does not name the PR", rec.Body.String())
	}
	if got := ms.beads["gd-task"].Status; got != model.StatusInProgress {
		t.Errorf("status = %q, want in_progress", got)
	}
	// A PATCH that does not close is untouched.
	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/gd-task", map[string]any{"status": "open"}), http.StatusOK)
}

func TestGRPCCloseBead_OpenPR_FailedPrecondition(t *testing.T) {
	s, ms, _, _ := newOpenPRGuardServer("open")
	seedPRTask(ms, "gd-task", model.TypeTask)

	_, err := s.CloseBead(withAuthMethod(context.Background(), authMethodNone), &beadsv1.CloseBeadRequest{Id: "gd-task"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), testPRURL) {
		t.Fatalf("CloseBead err = %v, want FailedPrecondition naming the PR", err)
	}
}

// The merge escalation's close-on-execute release closes its requesting task
// even while the guard's lookup still reads the PR open (the bridge's
// projection trails the merge): the daemon's own close never meets the guard.
func TestOpenPRGuard_EscalationExecuteStillClosesTask(t *testing.T) {
	s, ms, _, _ := newOpenPRGuardServer("open")
	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"operation_type":        "gitlab_mr_merge",
		"requesting_task":       "gd-task",
		"close_requesting_task": true,
	})
	seedPRTask(ms, "gd-task", model.TypeTask)
	linkBlocks(ms, "gd-task", "gd-esc")

	s.afterBeadClosed(context.Background(), ms.beads["gd-esc"])

	if got := ms.beads["gd-task"].Status; got != model.StatusClosed {
		t.Fatalf("status = %q, want the executed merge escalation to close its task", got)
	}
}

// With no lookup set, the guard reads the github-bridge's merge-request bead.
func TestOpenPRGuard_DefaultReadsMergeRequestBead(t *testing.T) {
	for _, c := range []struct {
		mrState string
		want    int
	}{
		{"opened", http.StatusConflict},
		{"merged", http.StatusOK},
		{"closed", http.StatusOK},
		{"", http.StatusOK}, // no merge-request bead yet
	} {
		t.Run(c.mrState, func(t *testing.T) {
			_, ms, h := newTestServer()
			seedPRTask(ms, "gd-task", model.TypeTask)
			if c.mrState != "" {
				seedBead(ms, "gd-mr", mergeRequestType, model.StatusOpen, nil, map[string]any{
					"mr_url": testPRURL, "mr_state": c.mrState,
				})
			}
			requireStatus(t, doJSON(t, h, "POST", "/v1/beads/gd-task/close", nil), c.want)
		})
	}
}
