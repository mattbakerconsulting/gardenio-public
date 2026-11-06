package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// parseMRPillTemplate parses the mr_pill partial standalone so tests can
// render the named block directly. Mirrors the embed.FS lookup used in
// newServer() so the embedded template source is what's under test.
func parseMRPillTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("").ParseFS(templateFS, "templates/partials/mr_pill.html")
	if err != nil {
		t.Fatalf("parse mr_pill template: %v", err)
	}
	return tmpl
}

func renderMRPill(t *testing.T, v wrapUpMRView) string {
	t.Helper()
	tmpl := parseMRPillTemplate(t)
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "mr_pill", v); err != nil {
		t.Fatalf("execute mr_pill: %v", err)
	}
	return buf.String()
}

// TestMRPill_FullPill exercises the happy path: every field populated.
// Pins the anatomy contract from ka-0000004478: href anchor, glyph, !iid,
// title, mr-state badge, and pipe-* chip.
func TestMRPill_FullPill(t *testing.T) {
	out := renderMRPill(t, wrapUpMRView{
		URL:            "https://gitlab.com/acme/platform/gardenio/-/merge_requests/2310",
		IID:            "2310",
		Title:          "feat(viewer): MR pill",
		State:          "opened",
		PipelineStatus: "success",
	})

	wants := []string{
		`href="https://gitlab.com/acme/platform/gardenio/-/merge_requests/2310"`,
		`target="_blank"`,
		`rel="noopener"`,
		`<a `,
		`class="mr-pill"`,
		`&#x2197;`, // ↗ glyph
		`!2310`,
		`feat(viewer): MR pill`,
		`mr-state mr-state-opened`,
		`>opened<`,
		`pipe-success`,
		`>success<`,
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("mr_pill output missing %q\noutput:\n%s", want, out)
		}
	}
}

// TestMRPill_StateAndPipelineCartesian renders the cartesian of mr_state ×
// pipeline_status palettes — pinning that each combination emits the right
// mr-state-<state> and pipe-<status> classes the CSS in type_mr.html keys on.
func TestMRPill_StateAndPipelineCartesian(t *testing.T) {
	states := []string{"opened", "merged", "closed", "draft", "unknown"}
	pipes := []string{"success", "failed", "running", "pending", "canceled", "skipped", "manual"}
	for _, state := range states {
		for _, pipe := range pipes {
			t.Run(state+"_"+pipe, func(t *testing.T) {
				out := renderMRPill(t, wrapUpMRView{
					URL:            "https://gitlab.com/x/-/merge_requests/1",
					IID:            "1",
					State:          state,
					PipelineStatus: pipe,
				})
				wantState := "mr-state-" + state
				wantPipe := "pipe-" + pipe
				if !strings.Contains(out, wantState) {
					t.Errorf("missing %q in:\n%s", wantState, out)
				}
				if !strings.Contains(out, wantPipe) {
					t.Errorf("missing %q in:\n%s", wantPipe, out)
				}
			})
		}
	}
}

// TestMRPill_DegradedMiss exercises the miss path: only URL + State="unknown".
// The pill must still anchor to the URL so operators get a deep-link even
// when the MR bead is absent.
func TestMRPill_DegradedMiss(t *testing.T) {
	out := renderMRPill(t, wrapUpMRView{
		URL:   "https://gitlab.com/x/-/merge_requests/9999",
		State: "unknown",
	})
	if !strings.Contains(out, `href="https://gitlab.com/x/-/merge_requests/9999"`) {
		t.Errorf("degraded pill must still carry href anchor\noutput:\n%s", out)
	}
	if !strings.Contains(out, "mr-state-unknown") {
		t.Errorf("degraded pill must carry mr-state-unknown\noutput:\n%s", out)
	}
	// No IID, no title, no pipeline — verify those branches are skipped.
	if strings.Contains(out, "mr-pill-iid") {
		t.Errorf("degraded pill should not emit iid span\noutput:\n%s", out)
	}
	if strings.Contains(out, "mr-pill-title") {
		t.Errorf("degraded pill should not emit title span\noutput:\n%s", out)
	}
	if strings.Contains(out, " pipe-") {
		t.Errorf("degraded pill should not emit pipeline chip\noutput:\n%s", out)
	}
}

// TestMRPill_TitleAttr verifies the hover-title fallback chain: IID+Title
// composes "!iid · title"; IID-only falls back to "!iid"; neither omits the
// attribute entirely.
func TestMRPill_TitleAttr(t *testing.T) {
	cases := []struct {
		name      string
		view      wrapUpMRView
		wantTitle string // substring expected in the title="..." attr; "" = no title attr
	}{
		{
			name:      "iid_and_title",
			view:      wrapUpMRView{URL: "u", IID: "42", Title: "Refactor auth"},
			wantTitle: `title="!42 · Refactor auth"`,
		},
		{
			name:      "iid_only",
			view:      wrapUpMRView{URL: "u", IID: "42"},
			wantTitle: `title="!42"`,
		},
		{
			name:      "title_only",
			view:      wrapUpMRView{URL: "u", Title: "Refactor auth"},
			wantTitle: `title="Refactor auth"`,
		},
		{
			name:      "neither",
			view:      wrapUpMRView{URL: "u"},
			wantTitle: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := parseMRPillTemplate(t)
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "mr_pill", tc.view); err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			if tc.wantTitle == "" {
				if strings.Contains(out, "title=") {
					t.Errorf("expected no title attr; got:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantTitle) {
				t.Errorf("missing %q in:\n%s", tc.wantTitle, out)
			}
		})
	}
}
