package wall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/spawnguard"
)

var now = time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)

type fakeSource struct {
	beads []*beadsapi.BeadDetail
	err   error
	// failType fails only the reads of that bead type.
	failType string
	calls    int
}

func (f *fakeSource) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.failType != "" && slices.Contains(q.Types, f.failType) {
		return nil, fmt.Errorf("%s read: connection refused", f.failType)
	}
	var match []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		if ok {
			match = append(match, b)
		}
	}
	if q.Offset >= len(match) {
		match = nil
	} else {
		match = match[q.Offset:]
	}
	return &beadsapi.ListBeadsResult{Beads: match, Total: len(match)}, nil
}

func (f *fakeSource) add(typ, title, status string, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID: fmt.Sprintf("kd-%03d", len(f.beads)+1), Type: typ, Title: title, Status: status, Labels: labels, Fields: fields,
	}
	f.beads = append(f.beads, b)
	return b
}

// farm is a tenant land "tomato-patch" granted 100 credits at x1.5 with $10
// spent (85 credits left), and the operator land "gardenio".
func farm() (*fakeSource, *beadsapi.BeadDetail, *beadsapi.BeadDetail) {
	f := &fakeSource{}
	land := f.add("project", "tomato-patch", "open", nil, map[string]string{"land_kind": "tenant", "account": "lands-tomato-patch"})
	f.add("project", "gardenio", "open", nil, map[string]string{"account": "fleet"})
	f.add("credit_rate", "v1", "closed", nil, map[string]string{"version": "1", "llm_multiplier": "1.5"})
	f.add("credit_grant", "g", "closed", []string{"project:tomato-patch"}, map[string]string{"land": "tomato-patch", "amount_credits": "100"})
	acct := f.add("account", "lands-tomato-patch", "open", nil, map[string]string{
		"total_cost_usd": "10", purse.FieldSpendSyncedAt: now.Add(-time.Minute).Format(time.RFC3339),
	})
	return f, land, acct
}

// check decides a lands-gardener spawn, the steward's own gardener.
func check(t *testing.T, f *fakeSource, project string, tenantSpawns bool) Refusal {
	t.Helper()
	return checkAs(t, f, project, "lands-gardener", tenantSpawns)
}

func checkAs(t *testing.T, f *fakeSource, project, role string, tenantSpawns bool) Refusal {
	t.Helper()
	r, err := Check(context.Background(), f, project, role, tenantSpawns, now)
	if err != nil {
		t.Fatalf("Check(%s, %s): %v", project, role, err)
	}
	return r
}

func TestCheck_TenantLandWithCreditsPasses(t *testing.T) {
	f, _, _ := farm()
	if r := check(t, f, "tomato-patch", true); r.Refused() {
		t.Fatalf("a tenant land with 85 credits left must pass, refused %+v", r)
	}
}

func TestCheck_Frozen(t *testing.T) {
	f, land, _ := farm()
	land.Fields["land_frozen"] = "true"
	// Frozen wins over tenant spawns being off and over a full purse.
	for _, on := range []bool{true, false} {
		r := check(t, f, "tomato-patch", on)
		if r.Code != CodeFrozen {
			t.Errorf("tenantSpawns=%v: code = %q, want %q", on, r.Code, CodeFrozen)
		}
	}
}

func TestCheck_LandsPauseGate(t *testing.T) {
	f, _, _ := farm()
	gate := f.add("gate", "lands-pause", "open", nil, map[string]string{})
	// The gate wins over tenant spawns being off and over a full purse.
	for _, on := range []bool{true, false} {
		if r := check(t, f, "tomato-patch", on); r.Code != CodeLandsPaused || !strings.Contains(r.Reason, gate.ID) {
			t.Errorf("tenantSpawns=%v: refusal = %+v, want %s naming %s", on, r, CodeLandsPaused, gate.ID)
		}
	}
	// A frozen land says frozen: the land's own switch is the more specific.
	f.beads[0].Fields["land_frozen"] = "true"
	if r := check(t, f, "tomato-patch", true); r.Code != CodeFrozen {
		t.Errorf("frozen and paused: code = %q, want %q", r.Code, CodeFrozen)
	}
	f.beads[0].Fields["land_frozen"] = ""
	// Operator lands never read the gate.
	if r := check(t, f, "gardenio", true); r.Refused() {
		t.Errorf("operator land refused by lands-pause: %+v", r)
	}
	// A closed gate, or one titled otherwise, does not pause.
	gate.Status = "closed"
	f.add("gate", "lands-pause-old", "open", nil, map[string]string{})
	if r := check(t, f, "tomato-patch", true); r.Refused() {
		t.Errorf("closed gate still pauses: %+v", r)
	}
}

func TestCheck_UnreadableLandsPauseGateRefusesTenantSpawns(t *testing.T) {
	f, _, _ := farm()
	f.failType = "gate"
	r := check(t, f, "tomato-patch", true)
	if r.Code != CodeLandsPaused || !strings.Contains(r.Reason, "fails closed") || !strings.Contains(r.Reason, "connection refused") {
		t.Fatalf("unreadable gate: refusal = %+v, want %s failing closed", r, CodeLandsPaused)
	}
	if r := check(t, f, "gardenio", true); r.Refused() {
		t.Errorf("an unreadable gate must not touch an operator land: %+v", r)
	}
}

func TestCheck_TenantSpawnsOff(t *testing.T) {
	f, _, _ := farm()
	r := check(t, f, "tomato-patch", false)
	if r.Code != CodeTenantSpawnsOff || !strings.Contains(r.Reason, EnvTenantSpawns) {
		t.Errorf("refusal = %+v, want %s naming %s", r, CodeTenantSpawnsOff, EnvTenantSpawns)
	}
}

// With tenant spawns off the steward, and only the steward, still starts
// (owner ruling gd-KOAoUELMWo).
func TestCheck_TenantSpawnsOffLetsOnlyTheStewardThrough(t *testing.T) {
	f, _, _ := farm()
	if r := checkAs(t, f, "tomato-patch", StewardRole, false); r.Refused() || !r.Tenant {
		t.Fatalf("steward with tenant spawns off: got %+v, want let through as a tenant land", r)
	}
	if r := checkAs(t, f, "tomato-patch", " steward ", false); r.Refused() {
		t.Errorf("steward role with spaces refused: %+v", r)
	}
	// Only the steward's exact role: not another role, not a role list
	// naming it beside another, not a name that starts or ends with it.
	for _, role := range []string{"lands-gardener", "crew", "gardener", "plan-gardener", "", "Steward",
		"crew,steward", "steward,crew", "stewards", "steward-x", "land-steward"} {
		if r := checkAs(t, f, "tomato-patch", role, false); r.Code != CodeTenantSpawnsOff {
			t.Errorf("role %q with tenant spawns off: got %+v, want %s", role, r, CodeTenantSpawnsOff)
		}
	}
	// With tenant spawns on, the steward and its gardeners both pass.
	for _, role := range []string{StewardRole, "lands-gardener", "crew"} {
		if r := checkAs(t, f, "tomato-patch", role, true); r.Refused() {
			t.Errorf("role %q with tenant spawns on refused: %+v", role, r)
		}
	}
	// An operator land is never refused, steward role or not.
	if r := checkAs(t, f, "gardenio", "crew", false); r.Refused() {
		t.Errorf("operator land refused: %+v", r)
	}
}

// The steward is exempt from tenant spawns off alone: every other refusal
// stops it, with tenant spawns on or off.
func TestCheck_StewardStillMeetsEveryOtherRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *fakeSource, land, acct *beadsapi.BeadDetail)
		want   string
	}{
		{"frozen", func(_ *fakeSource, land, _ *beadsapi.BeadDetail) { land.Fields["land_frozen"] = "true" }, CodeFrozen},
		{"lands paused", func(f *fakeSource, _, _ *beadsapi.BeadDetail) {
			f.add("gate", "lands-pause", "open", nil, map[string]string{})
		}, CodeLandsPaused},
		{"lands-pause unreadable", func(f *fakeSource, _, _ *beadsapi.BeadDetail) { f.failType = "gate" }, CodeLandsPaused},
		{"land gone", func(_ *fakeSource, land, _ *beadsapi.BeadDetail) { land.Status = "closed" }, CodeLandGone},
		{"purse at the wall", func(_ *fakeSource, _, acct *beadsapi.BeadDetail) { acct.Fields["total_cost_usd"] = "64" }, CodePurseWall},
		{"purse unknown", func(_ *fakeSource, _, acct *beadsapi.BeadDetail) {
			delete(acct.Fields, purse.FieldSpendSyncedAt)
		}, CodePurseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				f, land, acct := farm()
				tc.mutate(f, land, acct)
				if r := checkAs(t, f, "tomato-patch", StewardRole, on); r.Code != tc.want {
					t.Errorf("steward, tenantSpawns=%v: got %+v, want %s", on, r, tc.want)
				}
			}
		})
	}
	// A land that cannot be read is the caller's call, the steward's too.
	f, _, _ := farm()
	f.err = errors.New("daemon down")
	if r, err := Check(context.Background(), f, "tomato-patch", StewardRole, false, now); err == nil || r.Refused() {
		t.Errorf("unreadable land for the steward: got refusal %+v err %v, want the error", r, err)
	}
}

func TestCheck_PurseAtWall(t *testing.T) {
	f, _, acct := farm()
	acct.Fields["total_cost_usd"] = "64" // 96 credits spent: 4 left, under the 5-credit margin
	r := check(t, f, "tomato-patch", true)
	if r.Code != CodePurseWall {
		t.Fatalf("code = %q, want %q (%s)", r.Code, CodePurseWall, r.Reason)
	}
	if !strings.Contains(r.Reason, "4.00 credits remaining") {
		t.Errorf("reason %q should give the credits remaining", r.Reason)
	}
}

func TestCheck_PurseUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *fakeSource, acct *beadsapi.BeadDetail)
		want   string
	}{
		{"spend sync stale", func(_ *fakeSource, acct *beadsapi.BeadDetail) {
			acct.Fields[purse.FieldSpendSyncedAt] = now.Add(-20 * time.Minute).Format(time.RFC3339)
		}, "spend last synced"},
		{"never synced", func(_ *fakeSource, acct *beadsapi.BeadDetail) {
			delete(acct.Fields, purse.FieldSpendSyncedAt)
		}, "never stamped"},
		{"no rate in force", func(f *fakeSource, _ *beadsapi.BeadDetail) {
			f.beads = slices.DeleteFunc(f.beads, func(b *beadsapi.BeadDetail) bool { return b.Type == "credit_rate" })
		}, "no credit rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, acct := farm()
			tc.mutate(f, acct)
			r := check(t, f, "tomato-patch", true)
			if r.Code != CodePurseUnknown || !strings.Contains(r.Reason, tc.want) {
				t.Errorf("refusal = %+v, want %s mentioning %q", r, CodePurseUnknown, tc.want)
			}
		})
	}
}

// flakySource reads the projects and the gates, then fails every later read:
// the land is known to be a tenant's, but its purse cannot be read.
type flakySource struct{ *fakeSource }

func (f flakySource) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if slices.Contains(q.Types, "project") || slices.Contains(q.Types, "gate") {
		return f.fakeSource.ListBeadsFiltered(ctx, q)
	}
	return nil, errors.New("daemon down")
}

func TestCheck_PurseReadFailsIsUnknown(t *testing.T) {
	f, _, _ := farm()
	r, err := Check(context.Background(), flakySource{f}, "tomato-patch", "lands-gardener", true, now)
	if err != nil {
		t.Fatalf("a purse read failure is a refusal, not an error: %v", err)
	}
	if r.Code != CodePurseUnknown || !strings.Contains(r.Reason, "daemon down") {
		t.Errorf("refusal = %+v, want %s carrying the read error", r, CodePurseUnknown)
	}
}

func TestCheck_LandReadFailsIsError(t *testing.T) {
	f, _, _ := farm()
	f.err = errors.New("daemon down")
	r, err := Check(context.Background(), f, "tomato-patch", "lands-gardener", true, now)
	if err == nil || r.Refused() {
		t.Fatalf("an unreadable land is the caller's call: got refusal %+v err %v", r, err)
	}
	if u := Unknown("tomato-patch", err); u.Code != CodePurseUnknown || !strings.Contains(u.Reason, "daemon down") {
		t.Errorf("Unknown = %+v", u)
	}
}

func TestCheck_OperatorLandsUntouched(t *testing.T) {
	f, _, _ := farm()
	// The operator land is frozen-looking and the purse inputs are broken:
	// none of it applies to an operator land, with tenant spawns on or off.
	for _, b := range f.beads {
		if b.Title == "gardenio" {
			b.Fields["land_frozen"] = "true"
		}
	}
	f.beads = slices.DeleteFunc(f.beads, func(b *beadsapi.BeadDetail) bool { return b.Type == "credit_rate" })
	f.add("project", "kbeads", "open", nil, map[string]string{"land_kind": "operator"})
	for _, project := range []string{"gardenio", "kbeads", "no-such-project", ""} {
		for _, on := range []bool{true, false} {
			f.calls = 0
			if r := check(t, f, project, on); r.Refused() {
				t.Errorf("%q tenantSpawns=%v refused: %+v", project, on, r)
			}
			// A name no live project carries is also looked up as a
			// tenant land that has gone (closed projects, then accounts).
			want := 1
			if project == "no-such-project" {
				want = 3
			}
			if f.calls != want {
				t.Errorf("%q: %d daemon reads, want %d (no purse read)", project, f.calls, want)
			}
		}
	}
}

func TestNewSpawn(t *testing.T) {
	for state, want := range map[string]bool{
		"": true, "spawning": true, "queued": true, "budget_exceeded": true,
		"working": false, "idle": false, "rate_limited": false, "stopping": false, "done": false,
	} {
		if got := NewSpawn(state); got != want {
			t.Errorf("NewSpawn(%q) = %v, want %v", state, got, want)
		}
	}
}

type fakeRecorder struct {
	fields   map[string]map[string]string
	comments map[string][]string
	err      error
}

func newRecorder() *fakeRecorder {
	return &fakeRecorder{fields: map[string]map[string]string{}, comments: map[string][]string{}}
}

func (r *fakeRecorder) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if r.err != nil {
		return r.err
	}
	r.fields[id] = fields
	return nil
}

func (r *fakeRecorder) AddComment(_ context.Context, id, author, text string) error {
	if author != CommentAuthor {
		return fmt.Errorf("author %q", author)
	}
	r.comments[id] = append(r.comments[id], text)
	return nil
}

func TestRecord_StampsAndCommentsOnce(t *testing.T) {
	rec := newRecorder()
	r := Refusal{Code: CodePurseWall, Reason: "the purse is empty"}
	fields := map[string]string{"agent_state": "spawning"}
	wrote, err := Record(context.Background(), rec, "kd-ag1", fields, r)
	if err != nil || !wrote {
		t.Fatalf("Record = %v, %v", wrote, err)
	}
	got := rec.fields["kd-ag1"]
	if got["agent_state"] != "budget_exceeded" || got[FieldCode] != CodePurseWall || got[FieldReason] != "the purse is empty" {
		t.Errorf("fields = %v", got)
	}
	if c := rec.comments["kd-ag1"]; len(c) != 1 || !strings.Contains(c[0], CodePurseWall) || !strings.Contains(c[0], "the purse is empty") {
		t.Errorf("comments = %q", c)
	}

	// The bead now carries the refusal: re-deciding it writes nothing more.
	if wrote, err := Record(context.Background(), rec, "kd-ag1", got, r); wrote || err != nil {
		t.Errorf("second Record = %v, %v; want no write", wrote, err)
	}
	if len(rec.comments["kd-ag1"]) != 1 {
		t.Errorf("comments = %d, want 1", len(rec.comments["kd-ag1"]))
	}

	// A different reason (the land froze while it waited) is recorded.
	if wrote, _ := Record(context.Background(), rec, "kd-ag1", got, Refusal{Code: CodeFrozen, Reason: "frozen"}); !wrote {
		t.Error("a new refusal code must be recorded")
	}
	if len(rec.comments["kd-ag1"]) != 2 {
		t.Errorf("comments = %d, want 2", len(rec.comments["kd-ag1"]))
	}
}

func TestRecord_NoRefusalOrBeadWritesNothing(t *testing.T) {
	rec := newRecorder()
	for _, tc := range []struct {
		id string
		r  Refusal
	}{{"kd-ag1", Refusal{}}, {"", Refusal{Code: CodeFrozen}}} {
		if wrote, err := Record(context.Background(), rec, tc.id, nil, tc.r); wrote || err != nil {
			t.Errorf("Record(%q, %+v) = %v, %v", tc.id, tc.r, wrote, err)
		}
	}
	if len(rec.fields) != 0 {
		t.Errorf("fields = %v", rec.fields)
	}
}

func TestRecord_WriteError(t *testing.T) {
	rec := newRecorder()
	rec.err = errors.New("400")
	if _, err := Record(context.Background(), rec, "kd-ag1", nil, Refusal{Code: CodeFrozen}); err == nil {
		t.Fatal("a failed stamp must be reported")
	}
	if len(rec.comments) != 0 {
		t.Error("no comment without the stamp")
	}
}

func TestTenantSpawnsFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "nope": false, "true": true, "1": true, " TRUE ": true} {
		t.Setenv(EnvTenantSpawns, v)
		if got := TenantSpawnsFromEnv(); got != want {
			t.Errorf("%s=%q: %v, want %v", EnvTenantSpawns, v, got, want)
		}
	}
}

func TestCheck_MarksTenantLands(t *testing.T) {
	f, land, _ := farm()
	if r := check(t, f, "tomato-patch", true); !r.Tenant {
		t.Error("a passing tenant land must be marked Tenant, so the spawn rate applies")
	}
	if r := check(t, f, "gardenio", true); r.Tenant {
		t.Error("an operator land must not be marked Tenant")
	}
	land.Fields["land_frozen"] = "true"
	if r := check(t, f, "tomato-patch", true); !r.Tenant || r.Code != CodeFrozen {
		t.Errorf("a refused tenant land = %+v, want Tenant and %s", r, CodeFrozen)
	}
}

func TestRate_RefusesPastTheLandsCap(t *testing.T) {
	limit := spawnguard.NewKeyed(2, time.Hour)
	for _, id := range []string{"kd-a1", "kd-a2"} {
		if r := Rate(limit, "tomato-patch", id); r.Refused() || !r.Tenant {
			t.Fatalf("%s under the cap = %+v, want admitted", id, r)
		}
	}
	r := Rate(limit, "tomato-patch", "kd-a3")
	if r.Code != CodeSpawnRate || !strings.Contains(r.Reason, "cap of 2 agents in the last 1h0m0s") {
		t.Fatalf("third spawn in the hour = %+v, want %s naming the cap", r, CodeSpawnRate)
	}
	// The same agent decided again (the other door) keeps its slot.
	if r := Rate(limit, "tomato-patch", "kd-a1"); r.Refused() {
		t.Errorf("re-deciding an admitted agent refused it: %+v", r)
	}
	// Another land has its own cap.
	if r := Rate(limit, "bean-row", "kd-b1"); r.Refused() {
		t.Errorf("another land refused on tomato-patch's cap: %+v", r)
	}
}

func TestRate_NilLimitAdmits(t *testing.T) {
	if r := Rate(nil, "tomato-patch", "kd-1"); r.Refused() {
		t.Errorf("a nil limit refused: %+v", r)
	}
}

// A tenant land whose project bead is closed, deleted or renamed is still a
// tenant land: the wall fails closed on it rather than reading it as no land.
func TestCheck_GoneTenantLandFailsClosed(t *testing.T) {
	f, land, _ := farm()
	land.Status = "closed"
	for _, on := range []bool{true, false} {
		if r := check(t, f, "tomato-patch", on); r.Code != CodeLandGone || !r.Tenant || !strings.Contains(r.Reason, land.ID) {
			t.Errorf("closed tenant project, tenantSpawns=%v: got %+v, want %s naming %s", on, r, CodeLandGone, land.ID)
		}
	}

	// Deleted (or renamed away): the lands-<land> account still names it.
	f.beads = slices.DeleteFunc(f.beads, func(b *beadsapi.BeadDetail) bool { return b.ID == land.ID })
	if r := check(t, f, "tomato-patch", true); r.Code != CodeLandGone || !strings.Contains(r.Reason, "lands-tomato-patch") {
		t.Errorf("tenant land with only its account left: got %+v, want %s", r, CodeLandGone)
	}

	// A closed operator project is no tenant land.
	g := f.add("project", "old-tool", "closed", nil, map[string]string{"land_kind": "operator"})
	if r := check(t, f, "old-tool", true); r.Refused() {
		t.Errorf("closed operator project %s refused: %+v", g.ID, r)
	}

	// Not being able to tell is the caller's call (it fails closed).
	f.failType = "account"
	if r, err := Check(context.Background(), f, "tomato-patch", "lands-gardener", true, now); err == nil || r.Refused() {
		t.Errorf("unreadable accounts: got refusal %+v err %v, want the error", r, err)
	}
}
