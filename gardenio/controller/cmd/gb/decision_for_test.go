package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestNormalizeOperatedBy(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "matt@example.com", want: "oidc:matt@example.com"},
		{in: "Matt.Baker@acme.example", want: "oidc:matt.baker@acme.example"},
		{in: "U0AB1CD2EF", want: "slack:U0AB1CD2EF"},
		{in: "<@U0AB1CD2EF>", want: "slack:U0AB1CD2EF"},
		{in: "@U0AB1CD2EF", want: "slack:U0AB1CD2EF"},
		{in: "oidc:matt@example.com", want: "oidc:matt@example.com"},
		{in: "slack:U0AB1CD2EF", want: "slack:U0AB1CD2EF"},
		{in: "", wantErr: true},
		{in: "oidc:", wantErr: true},
		{in: "not-a-human", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeOperatedBy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeOperatedBy(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeOperatedBy(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeOperatedBy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// authorDaemon serves an agent bead whose operated_by is the FILING agent's
// operator, which is what makes a --for naming someone else a recognisable gift.
func authorDaemon(t *testing.T, authorOperator string) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-agent-author", "type": "agent", "status": "open",
			"fields": map[string]string{"operated_by": authorOperator},
		})
	}))
	old := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	return func() { daemon = old; srv.Close() }
}

// TestDecisionAddresseeFields_GiftsToAnotherHuman is the operator's exemption
// (thread ka-0000006045): "a user can author a decision that is operated_by
// someone else; essentially giving it to them." Addressing someone else is
// allowed outright — the only obligation is that the handover is recorded.
func TestDecisionAddresseeFields_GiftsToAnotherHuman(t *testing.T) {
	defer authorDaemon(t, "oidc:filing.agent@acme.example")()

	got, err := decisionAddresseeFields(context.Background(), "mr.author@acme.example", "gd-agent-author")
	if err != nil {
		t.Fatalf("decisionAddresseeFields: %v", err)
	}
	if got["operated_by"] != "oidc:mr.author@acme.example" {
		t.Errorf("operated_by = %q, want the --for target", got["operated_by"])
	}
	if got["gifted_by"] != "oidc:filing.agent@acme.example" {
		t.Errorf("gifted_by = %q, want the author — a gifted decision must say who handed it over", got["gifted_by"])
	}
}

// TestDecisionAddresseeFields_NoForInheritsAuthor asserts the default path
// stamps the author's own operator and is NOT recorded as a gift.
func TestDecisionAddresseeFields_NoForInheritsAuthor(t *testing.T) {
	defer authorDaemon(t, "oidc:filing.agent@acme.example")()

	got, err := decisionAddresseeFields(context.Background(), "", "gd-agent-author")
	if err != nil {
		t.Fatalf("decisionAddresseeFields: %v", err)
	}
	if got["operated_by"] != "oidc:filing.agent@acme.example" {
		t.Errorf("operated_by = %q, want the author's own operator", got["operated_by"])
	}
	if _, ok := got["gifted_by"]; ok {
		t.Errorf("filing a decision for yourself is not a gift, got gifted_by=%q", got["gifted_by"])
	}
}

// TestDecisionAddresseeFields_ForSelfIsNotAGift covers --for naming the author's
// own operator (a script that always passes --for): same addressee, no gift.
func TestDecisionAddresseeFields_ForSelfIsNotAGift(t *testing.T) {
	defer authorDaemon(t, "oidc:filing.agent@acme.example")()

	got, err := decisionAddresseeFields(context.Background(), "Filing.Agent@acme.example", "gd-agent-author")
	if err != nil {
		t.Fatalf("decisionAddresseeFields: %v", err)
	}
	if got["operated_by"] != "oidc:filing.agent@acme.example" {
		t.Errorf("operated_by = %q", got["operated_by"])
	}
	if _, ok := got["gifted_by"]; ok {
		t.Errorf("addressing yourself is not a gift, got gifted_by=%q", got["gifted_by"])
	}
}

// TestDecisionAddresseeFields_UnresolvableAuthorStillGifts asserts a create by a
// caller with no resolvable agent bead (a human at a terminal) still addresses
// the decision — the gift stamp is best-effort, the addressing is not.
func TestDecisionAddresseeFields_UnresolvableAuthorStillGifts(t *testing.T) {
	got, err := decisionAddresseeFields(context.Background(), "U0AB1CD2EF", "")
	if err != nil {
		t.Fatalf("decisionAddresseeFields: %v", err)
	}
	if got["operated_by"] != "slack:U0AB1CD2EF" {
		t.Errorf("operated_by = %q, want slack:U0AB1CD2EF", got["operated_by"])
	}
}

// TestDecisionAddresseeFields_BadForFailsLoud: an unparseable --for must fail the
// create rather than silently filing a decision addressed to nobody.
func TestDecisionAddresseeFields_BadForFailsLoud(t *testing.T) {
	if _, err := decisionAddresseeFields(context.Background(), "who?", ""); err == nil {
		t.Fatal("expected an error for an uninterpretable --for value")
	}
}
