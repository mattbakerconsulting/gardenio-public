package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestConsentToPushOptions(t *testing.T) {
	opts := consentToPushOptions()
	if len(opts) != 3 {
		t.Fatalf("consentToPushOptions() returned %d options, want 3", len(opts))
	}

	// Ids must be exactly the design-doc contract, in order, with a valid
	// artifact_type on each (the server rejects a missing/unknown one).
	wantIDs := []string{consentPushToMyMR, consentOpenSeparate, consentNoThanks}
	for i, opt := range opts {
		id, _ := opt["id"].(string)
		if id != wantIDs[i] {
			t.Errorf("option %d id = %q, want %q", i, id, wantIDs[i])
		}
		at, _ := opt["artifact_type"].(string)
		if !validArtifactTypes(at) {
			t.Errorf("option %q has invalid artifact_type %q", id, at)
		}
		if s, _ := opt["short"].(string); strings.TrimSpace(s) == "" {
			t.Errorf("option %q has empty short label", id)
		}
		if l, _ := opt["label"].(string); strings.TrimSpace(l) == "" {
			t.Errorf("option %q has empty label", id)
		}
	}
}

func TestRenderConsentContext(t *testing.T) {
	const (
		mr     = "https://gitlab.com/acme/app/-/merge_requests/123"
		branch = "ci-fix/gd-ABC-pipeline"
		target = "feature/authors-branch"
		report = "Changed .github/workflows/ci.yml: pin golangci-lint to v2.11.2 (+2/-2)."
	)

	got := renderConsentContext(mr, branch, target, report)

	// The body must carry every actionable pointer AND each option's execution
	// instruction, so both the DM reader and the respawned executor are covered.
	for _, want := range []string{mr, branch, target, report,
		consentPushToMyMR, consentOpenSeparate, consentNoThanks} {
		if !strings.Contains(got, want) {
			t.Errorf("renderConsentContext() missing %q\n---\n%s", want, got)
		}
	}

	// With a target branch, the push instruction names it.
	if !strings.Contains(got, "`"+target+"` and push to your MR") {
		t.Errorf("renderConsentContext() with target should name the target branch in the push instruction\n---\n%s", got)
	}

	// Without a target branch, the line degrades gracefully and never emits an
	// empty `` code span.
	noTarget := renderConsentContext(mr, branch, "", report)
	if strings.Contains(noTarget, "``") {
		t.Errorf("renderConsentContext() with no target emitted an empty code span:\n%s", noTarget)
	}
	if strings.Contains(noTarget, "Would land on") {
		t.Errorf("renderConsentContext() with no target should omit the 'Would land on' line:\n%s", noTarget)
	}
}

func TestValidateConsentInputs(t *testing.T) {
	const (
		goodMR = "https://gitlab.com/acme/app/-/merge_requests/1"
		branch = "ci-fix/x"
		human  = "author@example.com"
	)
	cases := []struct {
		name             string
		mr, branch, forH string
		wantErr          bool
	}{
		{name: "ok", mr: goodMR, branch: branch, forH: human},
		{name: "missing mr", mr: "", branch: branch, forH: human, wantErr: true},
		{name: "mr not a merge request", mr: "https://gitlab.com/acme/app/-/issues/1", branch: branch, forH: human, wantErr: true},
		{name: "missing branch", mr: goodMR, branch: "  ", forH: human, wantErr: true},
		{name: "missing for", mr: goodMR, branch: branch, forH: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConsentInputs(tc.mr, tc.branch, tc.forH)
			if tc.wantErr && err == nil {
				t.Errorf("validateConsentInputs(%q,%q,%q) = nil, want error", tc.mr, tc.branch, tc.forH)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateConsentInputs(%q,%q,%q) = %v, want nil", tc.mr, tc.branch, tc.forH, err)
			}
		})
	}
}

// Standing consent is the gated top-level user field only (gd-QV2WuUjely); the
// old preferences key, which any agent could write, is not consent.
func TestHasConsent(t *testing.T) {
	for name, tc := range map[string]struct {
		fields map[string]string
		want   bool
	}{
		"top-level field":         {map[string]string{"consent_push_to_mr_auto": "true"}, true},
		"withdrawn":               {map[string]string{"consent_push_to_mr_auto": "false"}, false},
		"preferences key ignored": {map[string]string{"preferences": `{"consent_push_to_mr_auto":true}`}, false},
		"unset":                   {map[string]string{}, false},
	} {
		if got := hasConsent(&beadsapi.BeadDetail{Fields: tc.fields}); got != tc.want {
			t.Errorf("%s: hasConsent = %v, want %v", name, got, tc.want)
		}
	}
	if hasConsent(nil) {
		t.Errorf("hasConsent(nil) = true")
	}
}

func TestAnsweredByStandingConsent(t *testing.T) {
	answered := &beadsapi.BeadDetail{Status: "closed", Fields: map[string]string{
		"chosen": consentPushToMyMR, "responded_by": standingConsentActor,
	}}
	if !answeredByStandingConsent(answered) {
		t.Errorf("daemon standing-consent answer not recognised")
	}
	for name, b := range map[string]*beadsapi.BeadDetail{
		"open":            {Status: "open", Fields: map[string]string{}},
		"human answer":    {Status: "closed", Fields: map[string]string{"chosen": consentPushToMyMR, "responded_by": "alice@example.com"}},
		"other option":    {Status: "closed", Fields: map[string]string{"chosen": consentNoThanks, "responded_by": standingConsentActor}},
		"bare withdrawal": {Status: "closed", Fields: map[string]string{}},
	} {
		if answeredByStandingConsent(b) {
			t.Errorf("%s: reported as a standing-consent answer", name)
		}
	}
}
