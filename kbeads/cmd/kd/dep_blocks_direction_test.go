package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/pflag"
)

// TestDepAdd_HelpDocumentsBlocksDirection locks the docs half of ka-0000004954:
// `kd dep add --help` must state which argument is the dependent and which is
// the blocker. Before this, the help was a bare `add <bead-id> <depends-on-id>`
// with no Long/Example, so "A blocks B" was a natural — and wrong — reading of
// `kd dep add A B --type blocks`. That misreading inverted a P0's blocks-edge
// and deadlocked a release (ka-0000000921).
func TestDepAdd_HelpDocumentsBlocksDirection(t *testing.T) {
	if depAddCmd.Long == "" {
		t.Fatal("depAddCmd.Long is empty — cobra prints only Short, dropping the direction guidance")
	}
	help := depAddCmd.Long + "\n" + depAddCmd.Example

	// The load-bearing claim: the stored semantics, in words. bead_id is the
	// dependent, depends_on_id the blocker (deps table convention — see
	// queryGetBlockedByForBeads / depRowForTypedVerb).
	for _, want := range []string{
		"A is BLOCKED BY B",
		"B must land first",
		"kd dep add A B --type blocks",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help must contain %q so the blocks direction is unambiguous; got:\n%s", want, help)
		}
	}

	// The help must also say what it does NOT mean — the specific inverted
	// reading that caused the incident.
	if !strings.Contains(help, `does NOT mean "A blocks B"`) {
		t.Errorf("help must explicitly reject the inverted reading; got:\n%s", help)
	}

	// And it must say the server will not auto-CORRECT a reversed blocks edge,
	// so the reader knows the echo is the check. Phrasing matters here: a blocks
	// CYCLE *is* caught server-side (internal/server/blocks_cycle_guard.go), so
	// claiming reversal "cannot be detected" would be false.
	if !strings.Contains(help, "cannot be auto-CORRECTED server-side") {
		t.Errorf("help must say a reversed blocks-edge is not auto-corrected; got:\n%s", help)
	}
	if strings.Contains(help, "CANNOT be detected server-side") {
		t.Errorf("help overclaims: a blocks CYCLE is detected server-side by the insert-time guard; got:\n%s", help)
	}

	// Recommend a read surface that can actually answer "what is this bead
	// waiting on?". `kd list` has no id filter and open_blockers[] omits closed
	// blockers, so pointing at it would send the reader somewhere that cannot
	// show a single bead's stored edges.
	if !strings.Contains(help, "kd dep list <bead-id>") {
		t.Errorf("help must point at `kd dep list <bead-id>` to inspect stored edges; got:\n%s", help)
	}
	if strings.Contains(help, "--with-blockers") {
		t.Errorf("help must not send the reader to `kd list --with-blockers` (no id filter; "+
			"omits closed blockers); got:\n%s", help)
	}
}

// TestDepAdd_ShortCarriesDirection keeps the direction in the Short line, which
// is the ONLY text `kd dep --help` prints for each subcommand. The reported
// failure was an agent needing three separate help probes (`kd dep`,
// `kd dep add`, `kd dep remove`) to reconstruct the direction, so the group
// listing — the first probe — has to answer it.
func TestDepAdd_ShortCarriesDirection(t *testing.T) {
	if !strings.Contains(depAddCmd.Short, "dependent first") {
		t.Errorf("depAddCmd.Short must carry the direction (it is all `kd dep --help` shows); got %q",
			depAddCmd.Short)
	}
}

// TestDepAdd_TypeFlagHelpCarriesDirection keeps the direction clause on the
// --type flag itself, not only in Long. `blocks` is the DEFAULT type, so the
// arg order is most often read by someone who never passed --type and may only
// ever skim the flag list.
func TestDepAdd_TypeFlagHelpCarriesDirection(t *testing.T) {
	for _, cmdName := range []string{"add", "remove"} {
		var usage string
		switch cmdName {
		case "add":
			usage = depAddCmd.Flags().Lookup("type").Usage
		case "remove":
			usage = depRemoveCmd.Flags().Lookup("type").Usage
		}
		if !strings.Contains(usage, "blocked BY") {
			t.Errorf("`dep %s` --type usage must carry the direction reminder; got %q", cmdName, usage)
		}
		// No backticks: cobra's UnquoteUsage() lifts the first backquoted span
		// out of the usage string and renders it as the flag's value
		// placeholder, so a backquoted example turns the help line into
		// `--type add A B -t blocks` instead of `--type string`. Caught by
		// running --help for real while writing this fix.
		if strings.Contains(usage, "`") {
			t.Errorf("`dep %s` --type usage must not contain backticks (cobra UnquoteUsage steals them "+
				"for the value placeholder); got %q", cmdName, usage)
		}
	}
}

// TestDepAdd_TypeFlagPlaceholderIsString is the direct assertion on the
// rendered help line: the --type flag must still advertise a string value.
func TestDepAdd_TypeFlagPlaceholderIsString(t *testing.T) {
	name, _ := pflag.UnquoteUsage(depAddCmd.Flags().Lookup("type"))
	if name != "string" {
		t.Errorf("--type value placeholder = %q, want \"string\" — a backquoted span in the usage "+
			"string hijacked it, rendering help as `--type %s`", name, name)
	}
}

// TestDepAdd_BlocksDirectionEcho is the runtime half of ka-0000004954: a
// successful `--type blocks` add prints the STORED direction in prose, so an
// inverted edge is caught by eye at add time. A reversed blocks-edge has no
// server-side tell (either orientation is legitimate), so this echo is the only
// point-of-use defense.
func TestDepAdd_BlocksDirectionEcho(t *testing.T) {
	mc := newMockClient() // default AddDependency echoes the request unchanged
	withMockClient(t, mc)
	setDepType(t, "blocks")

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-dependent", "gd-blocker"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	want := "NOTE: gd-dependent is now BLOCKED BY gd-blocker (gd-blocker must land first)"
	if !strings.Contains(out, want) {
		t.Fatalf("expected direction echo %q; got:\n%s", want, out)
	}

	// Last non-empty line, for the same reason as the parent-child flip note:
	// `kd dep add ... 2>&1 | tail -N` is ubiquitous and would otherwise drop it
	// (ka-0000007975).
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "NOTE: gd-dependent is now BLOCKED BY") {
		t.Errorf("direction echo must be the LAST stdout line (survives `tail -1`); last line was:\n%q\nfull:\n%s", last, out)
	}
}

// TestDepAdd_BlocksEcho_UsesServerReturnedEndpoints pins that the echo names the
// STORED edge, not what the caller typed. The other echo tests use the default
// mock, which returns the request unchanged — so dep.BeadID == args[0] there and
// printing the local beadID/dependsOnID variables instead would pass them all.
// Here the mock returns endpoints that differ from the request, so only the
// server-returned values can satisfy it.
func TestDepAdd_BlocksEcho_UsesServerReturnedEndpoints(t *testing.T) {
	mc := newMockClient()
	mc.AddDepFn = func(_, _, typ string) *model.Dependency {
		return &model.Dependency{
			BeadID:      "gd-stored-dependent",
			DependsOnID: "gd-stored-blocker",
			Type:        model.DependencyType(typ),
		}
	}
	withMockClient(t, mc)
	setDepType(t, "blocks")

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-typed-aaa", "gd-typed-bbb"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if !strings.Contains(out, "NOTE: gd-stored-dependent is now BLOCKED BY gd-stored-blocker") {
		t.Errorf("echo must name the SERVER-RETURNED endpoints; got:\n%s", out)
	}
	if strings.Contains(out, "gd-typed-aaa") || strings.Contains(out, "gd-typed-bbb") {
		t.Errorf("echo must not report the caller-typed ids as the stored edge; got:\n%s", out)
	}
}

// TestDepAdd_BlocksEcho_AllWaitEdgeSpellings covers the whole wait-edge
// allow-list, not just "blocks". blocked-by and depends-on store the SAME
// orientation and suppress readiness identically (blockingDepTypes in
// internal/store/postgres/queries.go), and `-t blocked-by` is precisely what a
// direction-unsure user reaches for — so it must get the echo too.
func TestDepAdd_BlocksEcho_AllWaitEdgeSpellings(t *testing.T) {
	for _, typ := range []string{"blocks", "blocked-by", "depends-on"} {
		t.Run(typ, func(t *testing.T) {
			mc := newMockClient()
			withMockClient(t, mc)
			setDepType(t, typ)

			out := captureStdout(t, func() {
				if err := depAddCmd.RunE(depAddCmd, []string{"gd-dependent", "gd-blocker"}); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})
			if !strings.Contains(out, "gd-dependent is now BLOCKED BY gd-blocker") {
				t.Errorf("--type %s is a wait edge and must get the direction echo; got:\n%s", typ, out)
			}
		})
	}
}

// TestIsWaitEdgeType_MirrorsBlockingDepTypes pins the predicate directly,
// including the structural types that must NOT be treated as wait edges.
func TestIsWaitEdgeType_MirrorsBlockingDepTypes(t *testing.T) {
	for _, typ := range []model.DependencyType{"blocks", "blocked-by", "depends-on"} {
		if !isWaitEdgeType(typ) {
			t.Errorf("isWaitEdgeType(%q) = false, want true (it suppresses readiness)", typ)
		}
	}
	for _, typ := range []model.DependencyType{
		model.DepParentChild, model.DepRelated, model.DepDuplicates,
		model.DepSupersedes, model.DepTriggeredBy, model.DepChainsTo, model.DepRates, "",
	} {
		if isWaitEdgeType(typ) {
			t.Errorf("isWaitEdgeType(%q) = true, want false (structural edge, not a wait edge)", typ)
		}
	}
}

// TestDepAdd_NoBlocksEcho_ForOtherTypes locks the negative: the blocks echo is
// type-scoped. parent-child has its own (flip) note and must not also claim a
// blocked-by relationship that was never stored.
func TestDepAdd_NoBlocksEcho_ForOtherTypes(t *testing.T) {
	for _, typ := range []string{"parent-child", "related"} {
		t.Run(typ, func(t *testing.T) {
			mc := newMockClient()
			withMockClient(t, mc)
			setDepType(t, typ)

			out := captureStdout(t, func() {
				if err := depAddCmd.RunE(depAddCmd, []string{"gd-aaa", "gd-bbb"}); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})
			if strings.Contains(out, "BLOCKED BY") {
				t.Errorf("blocks echo must not fire for --type %s; got:\n%s", typ, out)
			}
		})
	}
}

// TestDepAdd_BlocksEcho_SuppressedInJSON keeps the echo out of --json output,
// which must stay machine-parseable: the prose line would corrupt the document
// for `kd dep add ... --json | jq`.
func TestDepAdd_BlocksEcho_SuppressedInJSON(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setDepType(t, "blocks")
	jsonOutput = true // restored by withMockClient's cleanup

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-dependent", "gd-blocker"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if strings.Contains(out, "BLOCKED BY") {
		t.Errorf("prose echo must not appear in --json output; got:\n%s", out)
	}
}

// TestDepBlocksConstantMatchesFlagDefault guards the wiring the echo depends
// on: it fires on model.DepBlocks, but the flag default is the string
// "blocks". If either drifts, the echo silently stops firing on the default
// path — the exact silent-regression shape ka-0000004954 is about.
func TestDepBlocksConstantMatchesFlagDefault(t *testing.T) {
	if got := string(model.DepBlocks); got != "blocks" {
		t.Fatalf("model.DepBlocks = %q, want \"blocks\"", got)
	}
	if def := depAddCmd.Flags().Lookup("type").DefValue; def != string(model.DepBlocks) {
		t.Fatalf("--type default %q != model.DepBlocks %q — blocks echo would not fire by default",
			def, model.DepBlocks)
	}
}
