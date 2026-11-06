package bridge

import (
	"testing"

	"gardenio/controller/internal/podmanager"
)

func TestParseBackendFlag(t *testing.T) {
	mv := podmanager.ExecutionTargetLambdaMicroVM
	k8s := podmanager.ExecutionTargetKubernetes

	tests := []struct {
		name             string
		text             string
		wantTarget       string
		wantUnrecognized string
		wantRemaining    string
	}{
		{
			name:          "microvm alias",
			text:          "--backend=microvm build the feature",
			wantTarget:    mv,
			wantRemaining: "build the feature",
		},
		{
			name:          "lambda alias",
			text:          "--backend=lambda ship it",
			wantTarget:    mv,
			wantRemaining: "ship it",
		},
		{
			name:          "firecracker alias",
			text:          "--backend=firecracker",
			wantTarget:    mv,
			wantRemaining: "",
		},
		{
			name:          "kubernetes",
			text:          "--backend=kubernetes just answer",
			wantTarget:    k8s,
			wantRemaining: "just answer",
		},
		{
			name:          "k8s alias",
			text:          "--backend=k8s fix it",
			wantTarget:    k8s,
			wantRemaining: "fix it",
		},
		{
			name:          "pod alias",
			text:          "--backend=pod",
			wantTarget:    k8s,
			wantRemaining: "",
		},
		{
			name:          "case insensitive value",
			text:          "--backend=MicroVM go",
			wantTarget:    mv,
			wantRemaining: "go",
		},
		{
			// Recognized anywhere in the text (=value form has nothing to consume),
			// so it composes with the other flag parsers regardless of order.
			name:          "flag not at start",
			text:          "build the feature --backend=microvm",
			wantTarget:    mv,
			wantRemaining: "build the feature",
		},
		{
			// Composes with --ultracode in the same mention.
			name:          "alongside ultracode flag",
			text:          "--backend=microvm --ultracode=on do it",
			wantTarget:    mv,
			wantRemaining: "--ultracode=on do it",
		},
		{
			name:             "unrecognized value",
			text:             "--backend=vm do it",
			wantUnrecognized: "vm",
			wantRemaining:    "do it",
		},
		{
			// A bare "--backend=" with no value is left as prose and flagged.
			name:             "empty value",
			text:             "--backend= do it",
			wantUnrecognized: "--backend=",
			wantRemaining:    "do it",
		},
		{
			// Absent flag yields ("","",text) — critically NOT "kubernetes", so a
			// prior carried-forward lambda_microvm is never clobbered on a no-flag
			// wake-up.
			name:          "absent",
			text:          "just a normal prompt",
			wantTarget:    "",
			wantRemaining: "just a normal prompt",
		},
		{
			name:          "empty",
			text:          "",
			wantTarget:    "",
			wantRemaining: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, unrecognized, remaining := parseBackendFlag(tt.text)
			if target != tt.wantTarget {
				t.Errorf("target = %q, want %q", target, tt.wantTarget)
			}
			if unrecognized != tt.wantUnrecognized {
				t.Errorf("unrecognized = %q, want %q", unrecognized, tt.wantUnrecognized)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestBackendLabel(t *testing.T) {
	if got := backendLabel(podmanager.ExecutionTargetLambdaMicroVM); got != "Lambda MicroVM" {
		t.Errorf("microvm label = %q", got)
	}
	if got := backendLabel(podmanager.ExecutionTargetKubernetes); got != "Kubernetes pod" {
		t.Errorf("k8s label = %q", got)
	}
	if got := backendLabel("weird"); got != "weird" {
		t.Errorf("fallback label = %q", got)
	}
}
