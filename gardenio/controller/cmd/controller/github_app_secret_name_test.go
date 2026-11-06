package main

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestGithubTokenSecretName_MixedCaseBeadID: real bead ids are mixed case and
// the API server rejects a Secret name that is not an RFC 1123 subdomain. The
// fake clientset does not validate names, so the other tests could not catch
// this; rev 269 (2026-11-02) refused every GitHub spawn because of it.
func TestGithubTokenSecretName_MixedCaseBeadID(t *testing.T) {
	got := githubTokenSecretName("gd-OUtD8KYmIk")
	if want := "agent-github-token-gd-outd8kymik"; got != want {
		t.Fatalf("githubTokenSecretName() = %q, want %q", got, want)
	}
	if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
		t.Fatalf("githubTokenSecretName() = %q is not a valid Secret name: %v", got, errs)
	}
}

// TestGithubAppRefusalKey_OneCausePerHour: one cluster-wide mint failure
// refused every agent on 2026-11-02 and filed 14 escalations, one per agent
// bead. Refusals that differ only by agent must share a key; a different
// cause, project or hour must not.
func TestGithubAppRefusalKey_OneCausePerHour(t *testing.T) {
	at := time.Date(2026, 11, 2, 22, 29, 0, 0, time.UTC)
	reason := func(bead string) string {
		return `GitHub App installation token could not be minted for https://github.com/o/r ` +
			`(create github token secret agent-github-token-` + bead + `: Secret "agent-github-token-` +
			bead + `" is invalid: metadata.name: Invalid value)`
	}
	a, b := ghSpec(), ghSpec()
	a.AgentName, b.AgentName = "gardenio-merge-desk-ejb7", "gardenio-plan-gardener-for-gd-x07jyihezl-5ky7"

	k1 := githubAppRefusalKey(a, reason("gd-OUtD8KYmIk"), at)
	if k2 := githubAppRefusalKey(b, reason("gd-B9PpgW5uX3"), at.Add(18*time.Minute)); k1 != k2 {
		t.Errorf("same cause, two agents: keys differ\n%s\n%s", k1, k2)
	}
	if k := githubAppRefusalKey(a, "github app client: private key is not valid PEM", at); k == k1 {
		t.Error("a different cause must get its own key")
	}
	if k := githubAppRefusalKey(a, reason("gd-OUtD8KYmIk"), at.Add(time.Hour)); k == k1 {
		t.Error("the next hour must get a new key, so a cause that persists after a close files again")
	}
	c := a
	c.Project = "other"
	if k := githubAppRefusalKey(c, reason("gd-OUtD8KYmIk"), at); k == k1 {
		t.Error("a different project must get its own key")
	}
}
