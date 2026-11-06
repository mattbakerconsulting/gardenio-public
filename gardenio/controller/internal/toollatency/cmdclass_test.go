package toollatency

import "testing"

func TestClassifyCmd(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"plain git push", "git push origin main", "git_push"},
		{"path-qualified binary", "/usr/bin/git push origin main", "git_push"},
		{"unrecognized git subcommand", "git blame foo.go", "git_other"},
		{"git with no subcommand", "git", "git_other"},
		{"leading cd", "cd /work/repo && git push", "git_push"},
		{"leading cd semicolon", "cd /work/repo; make build", "make"},
		{"leading env assignment", "FOO=bar git commit -m wip", "git_commit"},
		{"chained cd and env", "cd /work && FOO=bar git push", "git_push"},
		{"make any target", "make preflight", "make"},
		{"kd allowlisted", "kd create \"foo\"", "kd_create"},
		{"kd other", "kd search foo", "kd_other"},
		{"non-allowlisted binary", "curl https://example.com", "other"},
		{"empty command", "", "other"},
		{"only env assignment", "FOO=bar", "other"},
		{"gh pr", "gh pr create --title x", "gh_pr"},
		{"docker run", "docker run --rm alpine", "docker_run"},
		{"kubectl logs", "kubectl logs -f pod/foo", "kubectl_logs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyCmd(tc.command); got != tc.want {
				t.Errorf("ClassifyCmd(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

// TestCmdClassDomainBounded pins the total cmd_class value count so a future
// change to the allowlist is a deliberate edit, not silent cardinality
// growth — the acceptance criterion (<2,000 total new series) depends on
// this staying small.
func TestCmdClassDomainBounded(t *testing.T) {
	domain := map[string]bool{ClassCmdOther: true, "make": true}
	for bin, subs := range cmdClassSubcommands {
		domain[bin+"_other"] = true
		for _, s := range subs {
			domain[bin+"_"+s] = true
		}
	}
	if len(domain) > 60 {
		t.Errorf("cmd_class domain has %d values, want <= 60 (keep it bounded, update this ceiling deliberately if the allowlist grows)", len(domain))
	}
}
