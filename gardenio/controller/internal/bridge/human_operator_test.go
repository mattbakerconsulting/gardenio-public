package bridge

import (
	"errors"
	"testing"
)

func TestIsHumanOperator(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Accepted human shapes.
		{"slack:U0OPERATOR1", true},
		{"slack:U12345", true},
		{"oidc:matthew.baker@acme.example", true},
		{"oidc:a@b.co", true},
		// Empty / sentinels / non-human providers.
		{"", false},
		{"system:gardenio-controller", false},
		{"system:machine", false},
		{"fleet:jira-bridge", false},
		{"jira:557058:abc-123", false},
		{"gitlab:4815162342", false},
		{"agent:gardenio-some-agent-x88", false},
		{"email:gardenio2@acme.example", false},
		// Malformed slack ids.
		{"slack:", false},
		{"slack:07K1", false},       // must start with U
		{"slack:Ufoo-bar", false},   // non-alnum
		{"slack:ulowercase", false}, // wrong case for leading char
		// Malformed oidc ids.
		{"oidc:", false},
		{"oidc:noatsign", false},
		{"oidc:@acme.example", false},   // empty local
		{"oidc:matt@", false},          // empty domain
		{"oidc:matt@localhost", false}, // domain not dotted
		{"oidc:matt@.com", false},      // leading dot
		{"oidc:matt@acme.", false}, // trailing dot
	}
	for _, c := range cases {
		if got := IsHumanOperator(c.in); got != c.want {
			t.Errorf("IsHumanOperator(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestParseHumanOperatorMode(t *testing.T) {
	cases := []struct {
		in   string
		want HumanOperatorMode
	}{
		{"off", HumanOperatorOff},
		{"OFF", HumanOperatorOff},
		{"disabled", HumanOperatorOff},
		{"0", HumanOperatorOff},
		{"false", HumanOperatorOff},
		{"enforce", HumanOperatorEnforce},
		{"ENFORCE", HumanOperatorEnforce},
		{"strict", HumanOperatorEnforce},
		{"refuse", HumanOperatorEnforce},
		{"warn", HumanOperatorWarn},
		{"", HumanOperatorWarn},     // default
		{"bogus", HumanOperatorWarn}, // unknown → safe default
		{"  warn  ", HumanOperatorWarn},
	}
	for _, c := range cases {
		if got := ParseHumanOperatorMode(c.in); got != c.want {
			t.Errorf("ParseHumanOperatorMode(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestGateSpawnOperator_HumanAlwaysProceeds(t *testing.T) {
	for _, mode := range []HumanOperatorMode{HumanOperatorOff, HumanOperatorWarn, HumanOperatorEnforce} {
		got, err := gateSpawnOperator(mode, discardLogger(), "test", "agent-x", "slack:U0OPERATOR1")
		if err != nil {
			t.Errorf("mode=%v: unexpected error for human operator: %v", mode, err)
		}
		if got != "slack:U0OPERATOR1" {
			t.Errorf("mode=%v: got %q; want the human operator unchanged", mode, got)
		}
	}
}

func TestGateSpawnOperator_NonHumanByMode(t *testing.T) {
	cases := []struct {
		name      string
		mode      HumanOperatorMode
		op        string
		wantErr   bool
		wantOpOut string
	}{
		{"off preserves empty", HumanOperatorOff, "", false, ""},
		{"off preserves jira sentinel", HumanOperatorOff, "jira:557058", false, "jira:557058"},
		{"warn preserves empty (allows)", HumanOperatorWarn, "", false, ""},
		{"warn preserves fleet sentinel (allows)", HumanOperatorWarn, "fleet:jira-bridge", false, "fleet:jira-bridge"},
		{"enforce refuses empty", HumanOperatorEnforce, "", true, ""},
		{"enforce refuses jira sentinel", HumanOperatorEnforce, "jira:557058", true, ""},
		{"enforce refuses system sentinel", HumanOperatorEnforce, "system:machine", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := gateSpawnOperator(c.mode, discardLogger(), "test-path", "agent-y", c.op)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected refusal error, got nil (op out=%q)", got)
				}
				var nhe *noHumanOperatorError
				if !errors.As(err, &nhe) {
					t.Fatalf("expected *noHumanOperatorError, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.wantOpOut {
				t.Errorf("got op %q; want %q", got, c.wantOpOut)
			}
		})
	}
}
