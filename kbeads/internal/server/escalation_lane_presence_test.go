package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

const laptopClient = "gardenio-laptop-operator"

// markLaneSeen writes client's last-seen row, as a request through
// coordinatorPresenceMiddleware would.
func markLaneSeen(ms *mockStore, client string, at time.Time) {
	val, _ := json.Marshal(map[string]string{"at": at.UTC().Format(time.RFC3339)})
	ms.configs[escalationLaneSeenKeyPrefix+client] = &model.Config{Key: escalationLaneSeenKeyPrefix + client, Value: val}
}

var laptopLane = laneRef{Kind: laneCoordinator, Name: "laptop", Raw: "coordinator:laptop"}

// gd-t41kYFbLUt: an allowlisted laptop client staffs coordinator:laptop only
// while it has made a request within the window; it may still hold.
func TestLaneStaffed_LaptopByPresence(t *testing.T) {
	p := defaultEscalationPolicy()
	esc := &model.Bead{ID: "gd-x", Type: model.TypeEscalation}
	for _, tc := range []struct {
		name string
		seen time.Duration // 0: never seen
		want bool
	}{
		{"never seen", 0, false},
		{"seen 31m ago", 31 * time.Minute, false},
		{"seen 5m ago", 5 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms := newLaneTestServer()
			if tc.seen > 0 {
				markLaneSeen(ms, laptopClient, time.Now().Add(-tc.seen))
			}
			if got, err := s.laneStaffed(context.Background(), laptopLane, esc, p); err != nil || got != tc.want {
				t.Fatalf("laneStaffed = %v, %v; want %v", got, err, tc.want)
			}
			if !s.laneMayHold(context.Background(), laptopLane, esc, p) {
				t.Fatal("laneMayHold = false; an allowlisted laptop may hold while away")
			}
			desk := laneRef{Kind: laneCoordinator, Name: "merge-desk", Raw: "coordinator:merge-desk"}
			if got, _ := s.laneStaffed(context.Background(), desk, esc, p); !got {
				t.Fatal("a role lane is staffed without presence")
			}
		})
	}
	t.Run("window 0 turns presence off", func(t *testing.T) {
		t.Setenv("BEADS_ESCALATION_LANE_PRESENCE_WINDOW", "0")
		s, _ := newLaneTestServer()
		if got, _ := s.laneStaffed(context.Background(), laptopLane, esc, p); !got {
			t.Fatal("presence off: the allowlist alone staffs the lane")
		}
	})
}

func TestCoordinatorPresenceMiddleware_RecordsLaptop(t *testing.T) {
	s, ms := newLaneTestServer()
	h := s.coordinatorPresenceMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	call := func(ctx context.Context) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/me", nil).WithContext(ctx))
	}
	call(machineCtx("gardenio-controller"))
	call(agentCtx("gd-rev1", "reviewer-a", "gardenio"))
	if len(ms.configs) != 0 {
		t.Fatalf("only a presence-tracked coordinator client is recorded; got %v", ms.configs)
	}
	call(machineCtx(laptopClient))
	if _, ok := ms.configs[escalationLaneSeenKeyPrefix+laptopClient]; !ok {
		t.Fatal("laptop request recorded no last-seen")
	}
	if got, _ := s.laneStaffed(context.Background(), laptopLane, &model.Bead{}, defaultEscalationPolicy()); !got {
		t.Fatal("laptop just seen: lane should be staffed")
	}
	delete(ms.configs, escalationLaneSeenKeyPrefix+laptopClient)
	call(machineCtx(laptopClient))
	if _, ok := ms.configs[escalationLaneSeenKeyPrefix+laptopClient]; ok {
		t.Fatal("a second request within a minute must not write again")
	}
}

// An offline laptop holds what its rules give it (D7 rule 5, gd-cuTEquiXqq), and the
// route is stamped once for the bridge's offline notice.
func TestEscalationRoute_HoldByLaptopStaysWhileOffline(t *testing.T) {
	s, ms := newLaneTestServer()
	markLaneSeen(ms, laptopClient, time.Now().Add(-2*time.Hour))
	for op, exec := range map[string]string{"minio_bucket_quota": "coordinator:laptop"} {
		raw, _ := json.Marshal(map[string]any{"operation_type": op})
		b := &model.Bead{ID: "gd-" + op, Type: model.TypeEscalation, Fields: raw, Labels: []string{"project:gardenio"}}
		f, err := s.escalationRouteFieldsFor(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		if f["held_by"] != "coordinator:laptop" || f["execute_by"] != exec {
			t.Fatalf("%s: held_by=%v execute_by=%v; want coordinator:laptop / %s", op, f["held_by"], f["execute_by"], exec)
		}
		if f[heldByOfflineAtField] == nil || f["held_by_last_seen_at"] == "" {
			t.Fatalf("%s: offline holder not stamped: %v", op, f)
		}
		b.Fields, _ = json.Marshal(f)
		again := map[string]any{"held_by": "coordinator:laptop"}
		s.noteOfflineHolder(context.Background(), b, again, defaultEscalationPolicy())
		if _, ok := again[heldByOfflineAtField]; ok {
			t.Fatalf("%s: the offline stamp is once per escalation", op)
		}
	}
}

// D5: an away laptop is passed over for a later agent lane, never for the owner.
func TestFirstStaffedLane_AwayLaptop(t *testing.T) {
	p := defaultEscalationPolicy()
	esc := &model.Bead{ID: "gd-x", Type: model.TypeEscalation}
	s, _ := newLaneTestServer()
	if got := s.firstStaffedLane(context.Background(), esc, []string{"coordinator:laptop", "operator"}, p); got != "coordinator:laptop" {
		t.Fatalf("laptop away, owner next: held_by = %s, want coordinator:laptop", got)
	}
	if got := s.firstStaffedLane(context.Background(), esc, []string{"coordinator:laptop", "coordinator:merge-desk", "operator"}, p); got != "coordinator:merge-desk" {
		t.Fatalf("laptop away, desk next: held_by = %s, want coordinator:merge-desk", got)
	}
	s, ms := newLaneTestServer()
	markLaneSeen(ms, laptopClient, time.Now())
	if got := s.firstStaffedLane(context.Background(), esc, []string{"coordinator:laptop", "coordinator:merge-desk", "operator"}, p); got != "coordinator:laptop" {
		t.Fatalf("laptop present: held_by = %s, want coordinator:laptop", got)
	}
}

// The idle sweep never hands sideways to an away laptop, and an approved item
// the away laptop executes stays with it.
func TestIdleHandUp_AwayLaptop(t *testing.T) {
	s, ms := newLaneTestServer()
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	old := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339)
	desk := mergeDeskEscalation("gd-crew1")
	desk["held_at"] = old
	seedRoutedEscalation(ms, "gd-desk", "gardenio", desk)
	seedRoutedEscalation(ms, "gd-run", "gardenio", map[string]any{
		"operation_type": "minio_bucket_quota", "approvers": []string{"operator"}, "approved_at": old, "approved_by": "oidc:o@example.com",
		"execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "held_at": old, "policy_rule": "escalation#5",
	})
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := escField(t, ms, "gd-desk", "held_by"); got == "coordinator:laptop" {
		t.Fatalf("desk idle with the laptop away: held_by = %v; the away laptop is not a hand-up target", got)
	}
	if got, exec := escField(t, ms, "gd-run", "held_by"), escField(t, ms, "gd-run", "execute_by"); got != "coordinator:laptop" || exec != "coordinator:laptop" {
		t.Fatalf("approved laptop work: held_by=%v execute_by=%v; want both coordinator:laptop", got, exec)
	}
}

func TestGetEscalation_HeldByPresence(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-lp", "gardenio", map[string]any{
		"operation_type": "minio_bucket_quota", "approvers": []string{"operator"},
		"execute_by": "coordinator:laptop", "held_by": "coordinator:laptop",
	})
	get := func() map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/escalations/gd-lp", nil).WithContext(operatorCtx("o@example.com"))
		r.SetPathValue("id", "gd-lp")
		rec := httptest.NewRecorder()
		s.handleGetEscalation(rec, r)
		var resp struct {
			Escalation struct {
				Presence map[string]any `json:"held_by_presence"`
			} `json:"escalation"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body)
		}
		return resp.Escalation.Presence
	}
	if lp := get(); lp == nil || lp["online"] != false || lp["lane"] != "coordinator:laptop" {
		t.Fatalf("never seen: held_by_presence = %v, want offline", lp)
	}
	s.recordCoordinatorSeen(machineCtx(laptopClient), time.Now().UTC())
	if lp := get(); lp["online"] != true || lp["last_seen_at"] == nil {
		t.Fatalf("just seen: held_by_presence = %v, want online", lp)
	}
}
