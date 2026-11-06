package mailproto

import "testing"

func TestSenderAndControlVerb(t *testing.T) {
	labels := []string{"from:slack-bridge", "control:kill"}
	if got := Sender(labels); got != "slack-bridge" {
		t.Fatalf("Sender = %q", got)
	}
	verb, ok := ControlVerb(labels)
	if !ok || verb != ControlKill {
		t.Fatalf("ControlVerb = %q,%v", verb, ok)
	}
	if got := Sender(nil); got != "unknown" {
		t.Fatalf("Sender(nil) = %q", got)
	}
	if _, ok := ControlVerb([]string{"from:x"}); ok {
		t.Fatal("no control label should report ok=false")
	}
}

func TestIsInterrupt(t *testing.T) {
	cases := []struct {
		labels   []string
		priority int
		want     bool
	}{
		{[]string{"delivery:interrupt"}, 2, true},
		{nil, 1, true},
		{nil, 0, true},
		{nil, 2, false},
		{[]string{"from:a"}, 3, false},
	}
	for _, c := range cases {
		if got := IsInterrupt(c.labels, c.priority); got != c.want {
			t.Errorf("IsInterrupt(%v,%d) = %v want %v", c.labels, c.priority, got, c.want)
		}
	}
}

func TestValidateLabels(t *testing.T) {
	if err := ValidateLabels([]string{"from:a", "control:kill", "control:escalation-approved"}); err != nil {
		t.Fatalf("known verbs must validate: %v", err)
	}
	if err := ValidateLabels([]string{"control:reboot"}); err == nil {
		t.Fatal("unknown control verb must be rejected")
	}
	if ControlLabel(ControlKill) != "control:kill" || FromLabel("me") != "from:me" {
		t.Fatal("label builders wrong")
	}
}
