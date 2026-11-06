package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestScopedAcceptance is the end-to-end statement of project-scoped human
// access (gd-6q0cRd9V7Q): the outcome the owner asked for, one row per line, run
// through the full HTTP stack (auth -> user provisioning -> role gate -> scoped
// route gate -> handler). The earlier tasks carry their own unit tests; this is
// the matrix a reviewer reads to see the whole promise in one place.
//
// Every refusal row runs twice on a fresh fixture: once as the scoped caller,
// and once as the control identity, which must NOT get the refusal. A row whose
// control also fails is a broken route, not a passing negative. The viewer half
// is TestScopedAcceptance in gardenio/controller/cmd/beads-viewer
// (scoped_acceptance_test.go), which cannot import this package.

// acceptanceRow is one line of the matrix.
type acceptanceRow struct {
	name  string
	token string // the identity under test
	setup func(f *scopedFixture)
	req   func(t *testing.T, f *scopedFixture, token string) (int, string)
	want  int // exact status for token; 0 means any 2xx

	// control is the positive control: the same request as this identity must
	// succeed (2xx). Empty means the row is itself a positive (allowed) row.
	control string
}

func (r acceptanceRow) ok(code int) bool {
	if r.want == 0 {
		return code/100 == 2
	}
	return code == r.want
}

func acceptGet(path string) func(*testing.T, *scopedFixture, string) (int, string) {
	return func(t *testing.T, f *scopedFixture, tok string) (int, string) {
		return f.do(t, tok, http.MethodGet, path, nil)
	}
}

func acceptApprove(id string) func(*testing.T, *scopedFixture, string) (int, string) {
	return func(t *testing.T, f *scopedFixture, tok string) (int, string) { return f.approve(t, tok, id) }
}

// Sub-resources the scoped read gate covers; the full list is pinned by
// TestScopedPrincipal_SubresourcesGated, repeated here so the matrix is whole.
var acceptanceSubresources = []string{
	"comments", "events", "mutations", "labels", "dependencies", "dependents",
	"followers", "mentions", "mentioned-by", "operator-history", "taints",
}

func acceptanceRows() []acceptanceRow {
	rows := []acceptanceRow{
		// scoped claimlayer | GET gd-1 (gardenio task) and every sub-resource | denied
		{name: "get gardenio bead", token: tokScopedClaimlayer, req: acceptGet("/v1/beads/" + beadGardenio), want: http.StatusNotFound, control: tokOwnerAdmin},
		// scoped claimlayer | GET an unlabelled bead | denied
		{name: "get unlabelled bead", token: tokScopedClaimlayer, req: acceptGet("/v1/beads/" + beadNoLabel), want: http.StatusNotFound, control: tokOwnerAdmin},
		{name: "get own bead", token: tokScopedClaimlayer, req: acceptGet("/v1/beads/" + beadClaimlayer), want: http.StatusOK},

		// scoped claimlayer | create a task with no project label | denied
		{name: "create task with no project label", token: tokScopedWriter, want: http.StatusForbidden, control: tokOwnerAdmin,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task"})
			}},
		{name: "create task labelled for another project", token: tokScopedWriter, want: http.StatusForbidden, control: tokOwnerAdmin,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task", "labels": []string{"project:gardenio"}})
			}},

		// scoped claimlayer | create/update/close a claimlayer task | allowed
		{name: "create claimlayer task", token: tokScopedWriter,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task", "labels": []string{"project:claimlayer"}})
			}},
		{name: "update claimlayer task", token: tokScopedWriter,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{"description": "d"})
			}},
		{name: "close claimlayer task", token: tokScopedWriter,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/close", map[string]any{})
			}},
		// ...and the same three against the gardenio task are denied.
		{name: "update gardenio task", token: tokScopedWriter, want: http.StatusNotFound, control: tokOwnerAdmin,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPatch, "/v1/beads/"+beadGardenio, map[string]any{"description": "d"})
			}},
		{name: "close gardenio task", token: tokScopedWriter, want: http.StatusNotFound, control: tokOwnerAdmin,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPost, "/v1/beads/"+beadGardenio+"/close", map[string]any{})
			}},

		// scoped claimlayer | approve a gardenio escalation | 403
		{name: "approve gardenio escalation", token: tokScopedWriter, want: http.StatusNotFound, control: tokOwnerAdmin,
			setup: func(f *scopedFixture) {
				f.seedEscalation("esc", "project:gardenio", "create", taskSpec("gardenio"), nil)
			},
			req: acceptApprove("esc")},
		// scoped claimlayer | approve a claimlayer infra-admin spawn | 403
		{name: "approve claimlayer infra-admin spawn", token: tokScopedWriter, want: http.StatusForbidden, control: tokOwnerAdmin,
			setup: func(f *scopedFixture) {
				makeRoleBead(f.ms, "gd-role-infra-admin", "infra-admin", true)
				f.seedEscalation("esc", "project:claimlayer", "create", map[string]any{
					"type": "agent", "title": "a", "labels": []string{"project:claimlayer"},
					"fields": map[string]any{"project": "claimlayer", "agent": "a", "role": "infra-admin"},
				}, nil)
			},
			req: acceptApprove("esc")},
		// scoped claimlayer | approve a claimlayer allowlisted escalation | 200
		{name: "approve claimlayer allowlisted escalation", token: tokScopedWriter, want: http.StatusOK,
			setup: func(f *scopedFixture) {
				f.seedEscalation("esc", "project:claimlayer", "create", taskSpec("claimlayer"), nil)
			},
			req: acceptApprove("esc")},
		// The escalation operation types a scoped principal never resolves.
		{name: "approve claimlayer merge_window escalation", token: tokScopedWriter, want: http.StatusForbidden, control: tokOwnerAdmin,
			setup: func(f *scopedFixture) {
				f.seedEscalation("esc", "project:claimlayer", "merge_window", map[string]any{"pr": "#1"}, nil)
			},
			req: acceptApprove("esc")},

		// scoped, no group | list and get | empty / denied
		{name: "no group: get own-project bead", token: tokScopedNoGroup, req: acceptGet("/v1/beads/" + beadClaimlayer), want: http.StatusNotFound, control: tokOwnerAdmin},
		{name: "no group: get gardenio bead", token: tokScopedNoGroup, req: acceptGet("/v1/beads/" + beadGardenio), want: http.StatusNotFound, control: tokOwnerAdmin},
		{name: "no group: create task", token: tokScopedNoGroup, want: http.StatusForbidden, control: tokOwnerAdmin,
			req: func(t *testing.T, f *scopedFixture, tok string) (int, string) {
				return f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task", "labels": []string{"project:claimlayer"}})
			}},
	}
	for _, sub := range acceptanceSubresources {
		rows = append(rows, acceptanceRow{
			name: "get gardenio bead /" + sub, token: tokScopedClaimlayer,
			req: acceptGet("/v1/beads/" + beadGardenio + "/" + sub), want: http.StatusNotFound, control: tokOwnerAdmin,
		}, acceptanceRow{
			name: "get own bead /" + sub, token: tokScopedClaimlayer,
			req: acceptGet("/v1/beads/" + beadClaimlayer + "/" + sub),
		})
	}
	return rows
}

func TestScopedAcceptance(t *testing.T) {
	for _, r := range acceptanceRows() {
		t.Run(r.name, func(t *testing.T) {
			run := func(token string) (int, string) {
				f := newScopedFixture(t)
				if r.setup != nil {
					r.setup(f)
				}
				return r.req(t, f, token)
			}
			code, body := run(r.token)
			if !r.ok(code) {
				t.Fatalf("%s: %d %s, want %d", r.name, code, body, r.want)
			}
			if r.control != "" {
				// Positive control: the same request, same state, as the owner.
				if code, body := run(r.control); code/100 != 2 {
					t.Errorf("control: %s as owner admin = %d %s, want 2xx; the row would pass on a broken route", r.name, code, body)
				}
			}
		})
	}

	t.Run("list beads: only project:claimlayer", func(t *testing.T) {
		f := newScopedFixture(t)
		if got := sortedKeys(f.listIDs(t, tokScopedClaimlayer)); !reflect.DeepEqual(got, []string{beadClaimlayer}) {
			t.Errorf("scoped claimlayer listed %v, want [%s]", got, beadClaimlayer)
		}
		// Control: the owner sees all four, so the cut is the gate's doing.
		if got := f.listIDs(t, tokOwnerAdmin); len(got) != 4 {
			t.Errorf("control: owner admin listed %v, want all four seeded beads", sortedKeys(got))
		}
	})

	t.Run("list beads: scoped with no group sees nothing", func(t *testing.T) {
		f := newScopedFixture(t)
		if got := f.listIDs(t, tokScopedNoGroup); len(got) != 0 {
			t.Errorf("no-group scoped listed %v, want nothing", sortedKeys(got))
		}
	})

	t.Run("search and filtered reads carry no other project's content", func(t *testing.T) {
		f := newScopedFixture(t)
		for id, b := range f.ms.beads {
			b.Title = "visible-" + id
			if id == beadGardenio || id == beadNoLabel || id == beadOther {
				b.Title = "leak-" + id
			}
		}
		for _, path := range []string{
			"/v1/beads?limit=100", "/v1/beads?limit=100&search=gd-1", "/v1/ready",
			"/v1/decisions", "/v1/escalations", "/v1/events?limit=100", "/v1/graph?limit=100",
			"/v1/batch/beads?ids=gd-1,nl-1,other-1",
		} {
			code, body := f.do(t, tokScopedClaimlayer, http.MethodGet, path, nil)
			if code/100 != 2 || strings.Contains(body, "leak-") {
				t.Errorf("scoped GET %s = %d, want 2xx without out-of-scope content: %s", path, code, body)
			}
		}
		// Control: the owner does see the content, so the check is not vacuous.
		if _, body := f.do(t, tokOwnerAdmin, http.MethodGet, "/v1/beads?limit=100", nil); !strings.Contains(body, "leak-") {
			t.Errorf("control: owner admin list lacks the leak- beads: %s", body)
		}
	})

	t.Run("operator (global): unchanged", func(t *testing.T) {
		f := newScopedFixture(t)
		if got := sortedKeys(f.listIDs(t, tokOperator)); len(got) != 4 {
			t.Errorf("operator listed %v, want all four seeded beads", got)
		}
		for _, id := range []string{beadGardenio, beadNoLabel, beadOther, beadClaimlayer} {
			if code, body := f.do(t, tokOperator, http.MethodGet, "/v1/beads/"+id, nil); code != http.StatusOK {
				t.Errorf("operator GET %s = %d %s, want 200", id, code, body)
			}
		}
		if code, body := f.do(t, tokOperator, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task"}); code/100 != 2 {
			t.Errorf("operator create without a project label = %d %s, want 2xx", code, body)
		}
		f.seedEscalation("gd-merge", "project:gardenio", "merge_window", map[string]any{"pr": "#1"}, nil)
		f.seedEscalation("gd-create", "project:gardenio", "create", taskSpec("gardenio"), nil)
		for _, id := range []string{"gd-merge", "gd-create"} {
			if code, body := f.approve(t, tokOperator, id); code != http.StatusOK {
				t.Errorf("operator approve %s = %d %s, want 200", id, code, body)
			}
		}
	})

	// The same bearer whose token groups lose /project-claimlayer: the next
	// request is the empty-list result (models the 300 s window).
	t.Run("group removal: the next request sees the empty result", func(t *testing.T) {
		f := newScopedFixture(t)
		if got := sortedKeys(f.listIDs(t, tokScopedClaimlayer)); !reflect.DeepEqual(got, []string{beadClaimlayer}) {
			t.Fatalf("control: before removal the bearer listed %v, want [%s]", got, beadClaimlayer)
		}
		if code, _ := f.do(t, tokScopedClaimlayer, http.MethodGet, "/v1/beads/"+beadClaimlayer, nil); code != http.StatusOK {
			t.Fatalf("control: before removal GET %s = %d, want 200", beadClaimlayer, code)
		}

		info := f.keycloak.tokens[tokScopedClaimlayer]
		info.groups = nil
		f.keycloak.tokens[tokScopedClaimlayer] = info

		if got := f.listIDs(t, tokScopedClaimlayer); len(got) != 0 {
			t.Errorf("after removal the bearer listed %v, want nothing", sortedKeys(got))
		}
		if code, body := f.do(t, tokScopedClaimlayer, http.MethodGet, "/v1/beads/"+beadClaimlayer, nil); code != http.StatusNotFound {
			t.Errorf("after removal GET %s = %d %s, want 404", beadClaimlayer, code, body)
		}
	})
}
