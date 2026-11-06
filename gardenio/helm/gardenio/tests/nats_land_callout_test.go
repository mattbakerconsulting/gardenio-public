package tests

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
)

var landCalloutFlags = append(slices.Clone(subjectIsolationFlags),
	"nats.auth.subjectIsolation.landCallout.enabled=true",
	"nats.auth.subjectIsolation.landCallout.issuerSecret=nats-land-issuer",
	"agents.enabled=true",
)

// With landCallout on, the rendered server config parses with the real
// nats-server parser into an auth callout that every user but infra and
// agent goes to, signed by the issuer the Secret's public key names
// (gd-sS7bSXOdGs). infra must bypass it: it is the callout's own connection.
func TestNatsLandCallout_ServerConf(t *testing.T) {
	conf := extractNatsServerConf(t, helmTemplate(t, "templates/nats/statefulset.yaml", landCalloutFlags...))
	acct, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := acct.PublicKey()
	t.Setenv("SERVER_NAME", "test-nats-0")
	t.Setenv("NATS_INFRA_TOKEN", "infra-secret")
	t.Setenv("NATS_AGENT_TOKEN", "agent-secret")
	t.Setenv("NATS_LAND_CALLOUT_ISSUER", pub)
	path := filepath.Join(t.TempDir(), "nats-server.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("nats-server rejects the landCallout config:\n%v\n--- config ---\n%s", err, conf)
	}
	if opts.AuthCallout == nil {
		t.Fatal("no auth_callout parsed")
	}
	if opts.AuthCallout.Issuer != pub {
		t.Errorf("auth_callout issuer = %q, want the env-substituted %q", opts.AuthCallout.Issuer, pub)
	}
	if got := slices.Sorted(slices.Values(opts.AuthCallout.AuthUsers)); !slices.Equal(got, []string{"agent", "infra"}) {
		t.Errorf("auth_callout auth_users = %v, want [agent infra]", got)
	}
	if len(opts.Users) != 2 {
		t.Errorf("parsed %d users, want infra + agent", len(opts.Users))
	}

	sts := helmTemplate(t, "templates/nats/statefulset.yaml", landCalloutFlags...)
	if !strings.Contains(sts, "name: NATS_LAND_CALLOUT_ISSUER") || !strings.Contains(sts, "name: nats-land-issuer") {
		t.Error("NATS container lacks NATS_LAND_CALLOUT_ISSUER from the issuer Secret")
	}
}

// Off (the default), there is no callout and no issuer env.
func TestNatsLandCallout_OffByDefault(t *testing.T) {
	sts := renderWithIsolation(t, "templates/nats/statefulset.yaml")
	if strings.Contains(sts, "auth_callout") || strings.Contains(sts, "NATS_LAND_CALLOUT_ISSUER") {
		t.Error("landCallout off renders an auth_callout or its issuer")
	}
	ctrl := renderWithIsolation(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	if strings.Contains(ctrl, "nats-land-callout") || strings.Contains(ctrl, "NATS_LAND_CALLOUT_ISSUER_SEED_FILE") {
		t.Error("landCallout off mounts the callout seed into the controller")
	}
}

// The controller, which answers the callout, gets the issuer seed as a file
// and only the seed key; nothing else mounts it.
func TestNatsLandCallout_ControllerGetsTheSeed(t *testing.T) {
	ctrl := helmTemplate(t, "templates/controller/deployment.yaml", landCalloutFlags...)
	for _, want := range []string{
		"name: NATS_LAND_CALLOUT_ISSUER_SEED_FILE",
		"value: /var/run/secrets/nats-land-callout/seed",
		"mountPath: /var/run/secrets/nats-land-callout",
		"secretName: nats-land-issuer",
	} {
		if !strings.Contains(ctrl, want) {
			t.Errorf("controller lacks %q", want)
		}
	}
	vol := ctrl[strings.Index(ctrl, "- name: nats-land-callout\n          secret:"):]
	vol = vol[:strings.Index(vol, "path: seed")]
	if strings.Contains(vol, "key: public") || !strings.Contains(vol, "key: seed") {
		t.Errorf("controller callout volume must project the seed key alone:\n%s", vol)
	}
}
