package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/heraldry"
	"gardenio/kbeads/internal/model"
)

// Arms over gRPC, undo and the library route (heraldry spec §3.1, step 1.2
// gd-ljbF6ftzdY). The gRPC tests wire the real client.GRPCClient to the real
// BeadsServer (newGRPCRoundtripClient), so a proto field either half forgets
// fails here. The HTTP shape is pinned in heraldry_test.go.

func TestGRPC_Heraldry_CreateGetListRoundTrip(t *testing.T) {
	c, ms := newGRPCRoundtripClient(t)
	ctx := context.Background()

	armed, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "Armed plan", Type: "epic", Heraldry: "tl:🔥 c:🧭 br:📦", Labels: []string{"project:alpha"}})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if armed.Heraldry != armsRun {
		t.Fatalf("create heraldry = %q, want %q (the run)", armed.Heraldry, armsRun)
	}
	if got := ms.beads[armed.ID].Heraldry; got != armsRun {
		t.Fatalf("stored heraldry = %q, want %q: CreateBeadRequest.heraldry was dropped on the wire", got, armsRun)
	}
	got, err := c.GetBead(ctx, armed.ID)
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.Heraldry != armsRun {
		t.Fatalf("GetBead heraldry = %q, want %q: Bead.heraldry was dropped", got.Heraldry, armsRun)
	}

	reordered, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "Reordered plan", Type: "epic", Heraldry: armsOrder, Labels: []string{"project:alpha"}})
	if err != nil {
		t.Fatalf("CreateBead in another order: %v", err)
	}
	if _, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "Plain plan", Type: "epic", Labels: []string{"project:alpha"}}); err != nil {
		t.Fatalf("CreateBead without arms: %v", err)
	}

	// The run matches exactly; the order is part of the arms.
	for _, tc := range []struct {
		filter string
		want   []string
	}{
		{armsRun, []string{armed.ID}},
		{armsOrder, []string{reordered.ID}},
	} {
		list, err := c.ListBeads(ctx, &client.ListBeadsRequest{Heraldry: tc.filter})
		if err != nil {
			t.Fatalf("ListBeads heraldry=%q: %v", tc.filter, err)
		}
		var ids []string
		for _, b := range list.Beads {
			ids = append(ids, b.ID)
		}
		slices.Sort(ids)
		slices.Sort(tc.want)
		if !slices.Equal(ids, tc.want) {
			t.Errorf("ListBeads heraldry=%q returned %v, want %v: a dropped filter returns the wider set", tc.filter, ids, tc.want)
		}
	}
	if _, err := c.ListBeads(ctx, &client.ListBeadsRequest{Heraldry: "🌱🔥📦"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListBeads with arms outside the library: code = %v (err %v), want InvalidArgument", status.Code(err), err)
	}
}

// seedEpic puts an epic of project alpha in the mock store, its labels in the
// label table too (the store reads a bead's labels from there).
func seedEpic(ms *mockStore, id, arms string) *model.Bead {
	b := &model.Bead{ID: id, Kind: model.KindIssue, Type: model.TypeEpic, Title: id, ShortTitle: id, Status: model.StatusOpen, Heraldry: arms, Labels: []string{"project:alpha"}}
	ms.beads[id] = b
	ms.labels[id] = []string{"project:alpha"}
	return b
}

func TestGRPC_Heraldry_UpdateAndRemove(t *testing.T) {
	c, ms := newGRPCRoundtripClient(t)
	ctx := context.Background()
	seedEpic(ms, "gd-plan", "slate 🔥 . 🧭 . 📦")

	// Re-giving the arms of a bead stored in the shield form stores the run.
	run := armsRun
	b, err := c.UpdateBead(ctx, "gd-plan", &client.UpdateBeadRequest{Heraldry: &run})
	if err != nil {
		t.Fatalf("UpdateBead same emoji: %v", err)
	}
	if b.Heraldry != armsRun {
		t.Fatalf("heraldry = %q after re-giving the same emoji, want the run %q", b.Heraldry, armsRun)
	}

	// New arms replace; an unrelated update leaves them alone.
	next := "🎈🎨🎯"
	if b, err = c.UpdateBead(ctx, "gd-plan", &client.UpdateBeadRequest{Heraldry: &next}); err != nil {
		t.Fatalf("UpdateBead new arms: %v", err)
	}
	if b.Heraldry != next {
		t.Fatalf("heraldry = %q after a replace, want %q: UpdateBeadRequest.heraldry was dropped", b.Heraldry, next)
	}
	title := "Plan, retitled"
	if b, err = c.UpdateBead(ctx, "gd-plan", &client.UpdateBeadRequest{Title: &title}); err != nil {
		t.Fatalf("UpdateBead title: %v", err)
	}
	if b.Heraldry == "" {
		t.Fatal("an unrelated update removed the arms: optional heraldry read as set")
	}

	none := "none"
	if b, err = c.UpdateBead(ctx, "gd-plan", &client.UpdateBeadRequest{Heraldry: &none}); err != nil {
		t.Fatalf("UpdateBead none: %v", err)
	}
	if b.Heraldry != "" || ms.beads["gd-plan"].Heraldry != "" {
		t.Fatalf("heraldry = %q after none, want removed", b.Heraldry)
	}
}

func TestGRPC_Heraldry_ErrorCodes(t *testing.T) {
	c, ms := newGRPCRoundtripClient(t)
	ctx := context.Background()

	if _, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "First", Type: "epic", Heraldry: armsRun, Labels: []string{"project:alpha"}}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	_, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "Second", Type: "epic", Heraldry: armsLegacy, Labels: []string{"project:alpha"}})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("same arms in the same project: code = %v (err %v), want AlreadyExists", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, armsRun) || strings.Contains(msg, "🔥 . 🧭") {
		t.Errorf("conflict message %q must name the arms as the run, never the shield form", msg)
	}

	_, err = c.CreateBead(ctx, &client.CreateBeadRequest{Title: "A task", Type: "task", Heraldry: "🎈🎨🎯", Labels: []string{"project:alpha"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("arms on a task: code = %v (err %v), want InvalidArgument", status.Code(err), err)
	}

	seedEpic(ms, "gd-plan", "")
	twice := "🐝🐝🎈"
	if _, err = c.UpdateBead(ctx, "gd-plan", &client.UpdateBeadRequest{Heraldry: &twice}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("UpdateBead with a charge twice: code = %v (err %v), want InvalidArgument", status.Code(err), err)
	}
	taken := armsRun
	if _, err = c.UpdateBead(ctx, "gd-plan", &client.UpdateBeadRequest{Heraldry: &taken}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("UpdateBead to taken arms: code = %v (err %v), want AlreadyExists", status.Code(err), err)
	}
	if got := ms.beads["gd-plan"].Heraldry; got != "" {
		t.Fatalf("stored heraldry = %q after refused updates, want none", got)
	}
}

func TestUndoMutation_Heraldry(t *testing.T) {
	old := "🎈🎨🎯"
	bead := &model.Bead{ID: "gd-undo-arms", Title: "Plan", Type: model.TypeEpic, Kind: model.KindIssue, Status: model.StatusOpen, Heraldry: armsRun, Labels: []string{"project:alpha"}}
	s, ms, mutID := newUndoFixture(t, "gd-undo-arms", bead, "heraldry", old, armsRun)
	ms.labels["gd-undo-arms"] = bead.Labels

	updated, _, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if err != nil {
		t.Fatalf("undoMutation: %v", err)
	}
	if updated.Heraldry != old {
		t.Fatalf("heraldry = %q after undo, want the old arms %q", updated.Heraldry, old)
	}

	// Undoing the first arms (old value "") removes them.
	seedEpic(ms, "gd-first-arms", "🧭🎈🎨")
	_ = ms.RecordMutation(context.Background(), &model.MutationRow{BeadID: "gd-first-arms", Field: "heraldry", OldValue: "", NewValue: "🧭🎈🎨", MutationType: model.MutationTypeUpdate})
	muts, _ := ms.ListMutations(context.Background(), "gd-first-arms")
	if updated, _, err = s.undoMutation(context.Background(), muts[0].ID, DefaultUndoRetentionDays); err != nil {
		t.Fatalf("undoMutation of the first arms: %v", err)
	}
	if updated.Heraldry != "" {
		t.Fatalf("heraldry = %q after undoing the first arms, want none", updated.Heraldry)
	}
}

func TestHandleUndo_HeraldryTakenSinceIs409(t *testing.T) {
	old := "🎈🎨🎯"
	bead := &model.Bead{ID: "gd-undo-arms", Title: "Plan", Type: model.TypeEpic, Kind: model.KindIssue, Status: model.StatusOpen, Heraldry: armsRun, Labels: []string{"project:alpha"}}
	s, ms, mutID := newUndoFixture(t, "gd-undo-arms", bead, "heraldry", old, armsRun)
	ms.labels["gd-undo-arms"] = bead.Labels
	// Since the change, another bead of the project took the old arms.
	seedEpic(ms, "gd-holder", old)

	req := oidcCtxReq("POST", "/v1/mutations/"+strconv.FormatInt(mutID, 10)+"/undo", nil, "admin@example.com", []string{RoleAdmin})
	rec := httptest.NewRecorder()
	undoMux(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("undo onto taken arms: %d %s, want 409", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != heraldryConflictCode || body["bead_id"] != "gd-holder" {
		t.Errorf("409 body = %v, want code %s naming gd-holder", body, heraldryConflictCode)
	}
	if got := ms.beads["gd-undo-arms"].Heraldry; got != armsRun {
		t.Errorf("stored heraldry = %q after a refused undo, want unchanged %q", got, armsRun)
	}
}

func TestHandleHeraldryLibrary(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/heraldry/library", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/heraldry/library: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
		t.Errorf("Cache-Control = %q, want a day's cache", cc)
	}
	var lib heraldryLibraryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &lib); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(lib.Charges) != len(heraldry.Library()) || len(lib.Charges) < 1000 {
		t.Errorf("charges = %d, want the whole library (%d)", len(lib.Charges), len(heraldry.Library()))
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, gone := range []string{"tinctures", "places"} {
		if _, ok := raw[gone]; ok {
			t.Errorf("the library still serves %q; arms are a run, with no colour or places", gone)
		}
	}
	if lib.ArtworkVersion != heraldry.NotoEmojiTag || lib.Help != model.HeraldryHelp {
		t.Errorf("artwork_version %q, help %q", lib.ArtworkVersion, lib.Help)
	}
	byEmoji := map[string]heraldryLibraryCharge{}
	for _, ch := range lib.Charges {
		byEmoji[ch.Emoji] = ch
	}
	for _, want := range heraldry.Library()[:3] {
		if got := byEmoji[want.Emoji]; got.Name != want.Name || got.Codepoints != want.Codepoints {
			t.Errorf("charge %s = %+v, want name %q codepoints %q", want.Emoji, got, want.Name, want.Codepoints)
		}
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req := httptest.NewRequest("GET", "/v1/heraldry/library", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation with the ETag: %d (%d bytes), want 304 and no body", rec.Code, rec.Body.Len())
	}
}
