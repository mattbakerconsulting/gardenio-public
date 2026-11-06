package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Tests for arms on gb's terminal lines (heraldry spec §3.3, step 1.4): the
// emoji run goes before the title in gb news, gb ready, gb plan show and gb
// bundle, and a bead with no arms prints exactly as before.

func TestWithArms(t *testing.T) {
	for _, tc := range []struct{ heraldry, title, want string }{
		{"", "Ship it", "Ship it"},
		{"blue 🌱 . 🔥 . 📦", "Ship it", "🌱🔥📦 Ship it"},
		{"sand . . 🧭 . .", "Find the way…", "🧭 Find the way…"},
	} {
		if got := withArms(tc.heraldry, tc.title); got != tc.want {
			t.Errorf("withArms(%q, %q) = %q, want %q", tc.heraldry, tc.title, got, tc.want)
		}
	}
}

// The run is added after the clip, so a long title is cut to its budget and
// the run is never counted or cut.
func TestNewsBeadLine_ArmsBeforeTitle(t *testing.T) {
	long := strings.Repeat("word ", 30)
	out := string(captureStdout(t, func() error {
		printNewsBead(&beadsapi.BeadDetail{ID: "gd-A", Type: "epic", Title: long, Heraldry: "blue 🌱 . 🔥 . 📦"})
		printNewsBead(&beadsapi.BeadDetail{ID: "gd-B", Type: "task", Title: "plain"})
		return nil
	}))
	if want := "gd-A [epic] — 🌱🔥📦 " + truncTitle(long, 72) + "  unassigned"; !strings.Contains(out, want) {
		t.Errorf("armed row missing %q in:\n%s", want, out)
	}
	if want := "gd-B [task] — plain  unassigned"; !strings.Contains(out, want) {
		t.Errorf("unarmed row missing %q in:\n%s", want, out)
	}
}

func TestResolvePlanRead_CarriesArmsAndShortTitle(t *testing.T) {
	f := &fakePlanReader{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-PLAN": {ID: "gd-PLAN", Type: "epic", Title: "Ship the heraldry", ShortTitle: "ship heraldry",
				Heraldry: "blue 🌱 . 🔥 . 📦", Status: "open",
				Fields: map[string]string{"phase_order": `["gd-PH1"]`}},
			"gd-PH1":  {ID: "gd-PH1", Type: "epic", Title: "The field", ShortTitle: "the field", Heraldry: "sand . . 🧭 . .", Status: "open"},
			"gd-S1":   {ID: "gd-S1", Type: "task", Title: "Phase step", ShortTitle: "phase step", Status: "open"},
			"gd-FLAT": {ID: "gd-FLAT", Type: "task", Title: "Flat step", ShortTitle: "flat step", Heraldry: "red . . 🐝 . .", Status: "closed"},
		},
		children: map[string][]string{
			"gd-PLAN": {"gd-PH1", "gd-FLAT"},
			"gd-PH1":  {"gd-S1"},
		},
	}
	view, err := resolvePlanRead(context.Background(), f, "gd-PLAN")
	if err != nil {
		t.Fatalf("resolvePlanRead: %v", err)
	}
	if view.ShortTitle != "ship heraldry" || view.Heraldry != "blue 🌱 . 🔥 . 📦" {
		t.Errorf("plan row = %q / %q", view.ShortTitle, view.Heraldry)
	}
	if len(view.Phases) != 1 || view.Phases[0].ShortTitle != "the field" || view.Phases[0].Heraldry != "sand . . 🧭 . ." {
		t.Fatalf("phase rows = %+v", view.Phases)
	}
	if st := view.Phases[0].Steps; len(st) != 1 || st[0].ShortTitle != "phase step" || st[0].Heraldry != "" {
		t.Errorf("phase step rows = %+v", st)
	}
	if len(view.Steps) != 1 || view.Steps[0].ShortTitle != "flat step" || view.Steps[0].Heraldry != "red . . 🐝 . ." {
		t.Fatalf("flat step rows = %+v", view.Steps)
	}

	out := string(captureStdout(t, func() error { printPlanReadView(view); return nil }))
	for _, want := range []string{
		"gd-PLAN  🌱🔥📦 Ship the heraldry  [open]",
		"Phase 1: gd-PH1  🧭 The field  [open]",
		"[ ] #1 gd-S1 (task) Phase step\n",
		"[x] #1 gd-FLAT (task) 🐝 Flat step\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan show missing %q in:\n%s", want, out)
		}
	}
}
