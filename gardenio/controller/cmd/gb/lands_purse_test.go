package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// purseView serves the fake store to the purse as the daemon would: projects
// and accounts as beads, plus every bead gb lands wrote, filtered by type and
// labels.
type purseView struct{ *fakeLandsStore }

func (v purseView) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	all := slices.Clone(v.beads)
	for name, p := range v.projects {
		all = append(all, &beadsapi.BeadDetail{ID: "proj-" + name, Type: "project", Title: name,
			Fields: map[string]string{"land_kind": p.LandKind, "account": p.Account}})
	}
	for name, a := range v.accounts {
		f := map[string]string{"litellm_key": a.LiteLLMKey}
		for k, val := range a.Fields {
			f[k] = val
		}
		all = append(all, &beadsapi.BeadDetail{ID: "acct-" + name, Type: "account", Title: name, Fields: f})
	}
	var out []*beadsapi.BeadDetail
	for _, b := range all {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		if ok && q.Offset == 0 {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

// A grant written by gb lands credit add is what the purse counts, at the
// rate gb lands rates set wrote.
func TestLandsPurse_ReadsTheLedger(t *testing.T) {
	st := newFakeLandsStore()
	seedRate(t, st, 1.5)
	rec := &budgetRecorder{}
	ctx := context.Background()
	if _, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 30, GrantedBy: "oidc:op"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	a := st.accounts["lands-tomato-patch"]
	a.Fields = map[string]string{"total_cost_usd": "8", purse.FieldSpendSyncedAt: now.Add(-3 * time.Minute).Format(time.RFC3339)}
	st.accounts["lands-tomato-patch"] = a

	got, err := readLandPurses(ctx, purseView{st}, []string{"tomato-patch"}, now)
	if err != nil {
		t.Fatal(err)
	}
	p := got[0]
	if p.GrantedCredits != 30 || *p.SpentCredits != 12 || *p.RemainingCredits != 18 || p.State != purse.StateOK {
		t.Fatalf("purse = %+v; want 30 granted, 12 spent, 18 left, ok", p)
	}
	if *p.GrantedUSD != rec.set["sk-tomato"] {
		t.Errorf("granted at cost $%v must equal the key's max_budget $%v", *p.GrantedUSD, rec.set["sk-tomato"])
	}

	var buf bytes.Buffer
	printPurse(&buf, p)
	for _, want := range []string{"tomato-patch: ok", "18.00", "$12.00", "$8.00", "rate v1 x1.5"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}

	// No land named: every tenant land, and an unprovisioned one is unknown.
	all, err := readLandPurses(ctx, purseView{st}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.Land+"="+p.State)
	}
	if want := []string{"bean-row=unknown", "seedling=unknown", "tomato-patch=ok"}; !slices.Equal(names, want) {
		t.Errorf("all lands = %v, want %v", names, want)
	}
	buf.Reset()
	printPurse(&buf, all[0])
	if !strings.Contains(buf.String(), "new spawns refused") || !strings.Contains(buf.String(), "why") {
		t.Errorf("an unknown purse must say it blocks and why:\n%s", buf.String())
	}
}

func TestLandsPurse_OperatorLandHasNone(t *testing.T) {
	_, err := readLandPurses(context.Background(), purseView{newFakeLandsStore()}, []string{"gardenio"}, time.Now())
	if !errors.Is(err, purse.ErrOperatorLand) {
		t.Errorf("err = %v, want ErrOperatorLand", err)
	}
}
