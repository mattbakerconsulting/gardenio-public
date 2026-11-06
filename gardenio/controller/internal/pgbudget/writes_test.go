package pgbudget

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

var update = flag.Bool("update", false, "rewrite kbeads' pgbudget_writes.golden.json")

// writesGolden is shared with kbeads: TestBuiltinPgBudgetWritesValidate
// validates it against the daemon's builtin type:task and type:message
// schemas, which fakeStore does not enforce. It lives under kbeads/ so a
// change to it runs both CI arms (gd-ylgk1jBhLi).
const writesGolden = "../../../../kbeads/internal/server/testdata/pgbudget_writes.golden.json"

// goldenWrite is one CreateBead as the daemon validates it: the bead type and
// its fields, with the idempotency key where the daemon persists it.
type goldenWrite struct {
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields"`
}

// TestRunOnce_WritesMatchKbeadsGolden pins every bead field the filer and the
// report post write to the golden kbeads checks them against. Run with
// -update after changing a write, then run kbeads'
// TestBuiltinPgBudgetWritesValidate.
func TestRunOnce_WritesMatchKbeadsGolden(t *testing.T) {
	st := &fakeStore{now: asOf}
	st.put(&beadsapi.BeadDetail{ID: "gd-machines1", Type: "channel", Status: "open", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "machines", conversation.ChannelKind: conversation.ChannelKindMachines}})
	r := &Runner{Query: fixture(), Store: st, Budgets: DefaultBudgets(), Land: "gardenio", Epic: "gd-PkZRCb0BuB", Hour: 8, Actor: "gardenio-controller"}
	if err := r.RunOnce(context.Background(), asOf.Add(37*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var writes []goldenWrite
	for _, req := range st.reqs {
		if req.Type == "" {
			continue // seeded with put
		}
		var fields map[string]string
		if err := json.Unmarshal(req.Fields, &fields); err != nil {
			t.Fatalf("%s fields: %v", req.Type, err)
		}
		if req.IdempotencyKey != "" {
			fields["idempotency_key"] = req.IdempotencyKey
		}
		for k, v := range fields {
			if len([]rune(v)) > 40 {
				fields[k] = "(long)" // keeps report wording out of the golden
			}
		}
		writes = append(writes, goldenWrite{Type: req.Type, Fields: fields})
	}
	got, err := json.MarshalIndent(writes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(writesGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(writesGolden)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("writes differ from %s; run with -update and then kbeads' TestBuiltinPgBudgetWritesValidate:\ngot:\n%s\nwant:\n%s", writesGolden, got, want)
	}
}
