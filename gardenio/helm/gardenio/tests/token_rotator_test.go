package tests

import (
	"strings"
	"testing"
)

// Regression guards for ka-0000003722 — harden BEADS_DAEMON_TOKEN revocation
// list HA / fallback path.
//
// The rotator must:
//   1. expose a BEADS_HTTP_URL env var pointing at the in-cluster beads daemon
//      so the script can POST evicted hashes to /v1/admin/revoke-token (DB
//      persistence is the durable fallback for ConfigMap loss);
//   2. abort instead of overwriting the revoked-tokens ConfigMap when
//      `kubectl get` fails transiently (the previous version silently
//      truncated the list to a single hash on any non-zero exit).

func TestTokenRotator_BeadsHTTPURL_Wired(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/token-rotator/cronjob.yaml",
		"tokenRotation.enabled=true",
	)
	if !strings.Contains(out, "- name: BEADS_HTTP_URL") {
		t.Fatalf("expected BEADS_HTTP_URL env var on rotator container; got:\n%s", out)
	}
	// Default httpPort is 8080 and beads service name is <release>-beads.
	// The chart-test helper renders with release name "test".
	if !strings.Contains(out, `value: "http://test-beads:8080"`) {
		t.Errorf("expected BEADS_HTTP_URL to resolve to the in-cluster beads service on port 8080; got:\n%s", out)
	}
}

func TestTokenRotator_DBPersistencePost_Present(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/token-rotator/cronjob.yaml",
		"tokenRotation.enabled=true",
	)
	// The script must POST the evicted hash to the daemon's admin endpoint
	// using the still-live current token as bearer auth.
	if !strings.Contains(out, "/v1/admin/revoke-token") {
		t.Errorf("rotator script must POST evicted hashes to /v1/admin/revoke-token; got:\n%s", out)
	}
	if !strings.Contains(out, "Authorization: Bearer $CURRENT_TOKEN") {
		t.Errorf("rotator script must authenticate the revoke-token POST with the current daemon token; got:\n%s", out)
	}
}

func TestTokenRotator_TransientErrorAborts(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/token-rotator/cronjob.yaml",
		"tokenRotation.enabled=true",
	)
	// The script must distinguish NotFound (legitimate first run) from a
	// transient apiserver error (must NOT proceed — would truncate the list).
	if !strings.Contains(out, "NotFound") {
		t.Errorf("rotator script must distinguish NotFound from transient errors when reading the revoked-tokens ConfigMap; got:\n%s", out)
	}
	if !strings.Contains(out, "Aborting rotation to avoid destructive overwrite") {
		t.Errorf("rotator script must abort on transient ConfigMap read failure; got:\n%s", out)
	}
}

func TestTokenRotator_HTTPPushBestEffort(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/token-rotator/cronjob.yaml",
		"tokenRotation.enabled=true",
	)
	// A failed DB push must NOT abort rotation — the ConfigMap path is the
	// fallback. Look for the WARN log line, which is the contract.
	if !strings.Contains(out, "WARN: beads DB revoke-token push failed") {
		t.Errorf("rotator script must log a WARN (not abort) when the DB push fails; got:\n%s", out)
	}
}
