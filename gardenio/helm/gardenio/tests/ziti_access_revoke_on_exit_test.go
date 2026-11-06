package tests

import (
	"strings"
	"testing"
)

// Regression (gd-qYD8piysYM): the controller's ZITI_ACCESS_REVOKE_ON_EXIT env
// var must reflect an operator's `agents.zitiAccess.revokeOnExit=false`.
//
// The template previously read
//   value: {{ .Values.agents.zitiAccess.revokeOnExit | default true | quote }}
// while the adjacent guard that mounts the broker-audience revoke token read
// the RAW value (`{{- if .Values.agents.zitiAccess.revokeOnExit }}`). Sprig's
// `default` treats boolean `false` as empty and forced the env var back to
// "true", so with revokeOnExit=false the controller was told to revoke Ziti
// leases on exit yet the token needed to authenticate those DELETE /lease calls
// was NOT mounted — a guaranteed revoke failure. The fix references the value
// directly so the env var and the mount guard always agree.

func zitiAccessFlags(extra ...string) []string {
	base := []string{
		"agents.enabled=true",
		"agents.zitiAccess.enabled=true",
		"agents.zitiAccess.brokerURL=https://broker.example.com",
	}
	return append(base, extra...)
}

func TestZitiAccess_RevokeOnExit_DefaultTrue(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml", zitiAccessFlags()...)
	if !strings.Contains(out, `name: ZITI_ACCESS_REVOKE_ON_EXIT`) {
		t.Fatalf("ZITI_ACCESS_REVOKE_ON_EXIT env must render when zitiAccess enabled, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "true"`) {
		t.Errorf("default revokeOnExit must render ZITI_ACCESS_REVOKE_ON_EXIT=true, got:\n%s", out)
	}
	// Env true => the broker-audience revoke token MUST be mounted.
	if !strings.Contains(out, "ZITI_ACCESS_CONTROLLER_TOKEN_FILE") {
		t.Errorf("revokeOnExit=true must mount the broker-audience token (ZITI_ACCESS_CONTROLLER_TOKEN_FILE), got:\n%s", out)
	}
}

func TestZitiAccess_RevokeOnExit_FalseHonored(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		zitiAccessFlags("agents.zitiAccess.revokeOnExit=false")...,
	)
	// The env var must reflect false, not be clobbered back to "true" by the
	// `| default true` bug.
	envIdx := strings.Index(out, "name: ZITI_ACCESS_REVOKE_ON_EXIT")
	if envIdx < 0 {
		t.Fatalf("ZITI_ACCESS_REVOKE_ON_EXIT env must render when zitiAccess enabled, got:\n%s", out)
	}
	after := out[envIdx:]
	if !strings.Contains(after[:min(200, len(after))], `value: "false"`) {
		t.Errorf("revokeOnExit=false must render ZITI_ACCESS_REVOKE_ON_EXIT=false (not clobbered to true), got:\n%s", after[:min(200, len(after))])
	}
	// Env false => the token guard must NOT mount the revoke token (they must
	// agree; the pre-fix mismatch mounted nothing while the env said "true").
	if strings.Contains(out, "ZITI_ACCESS_CONTROLLER_TOKEN_FILE") {
		t.Errorf("revokeOnExit=false must NOT mount the broker-audience token, got:\n%s", out)
	}
}
