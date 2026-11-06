package githubapp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestIdentitiesByName(t *testing.T) {
	_, pemBytes := genKey(t)
	fleet, err := New(Config{AppID: "1", PrivateKey: pemBytes})
	if err != nil {
		t.Fatal(err)
	}
	lands, err := New(Config{Name: Lands, AppID: "2", PrivateKey: pemBytes})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := NewIdentities(fleet, nil, lands)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ids.App(Fleet); got != fleet {
		t.Errorf("App(fleet) is not the fleet client")
	}
	if got, _ := ids.App(Lands); got != lands {
		t.Errorf("App(lands) is not the lands client")
	}

	// Only the fleet configured: asking for lands is ErrNotConfigured, never the fleet client.
	only, _ := NewIdentities(fleet)
	if c, err := only.App(Lands); c != nil || !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("App(lands) with only the fleet = %v, %v; want nil, ErrNotConfigured", c, err)
	}
	var none *Identities
	if _, err := none.App(Fleet); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil Identities = %v", err)
	}

	if _, err := NewIdentities(fleet, fleet); err == nil {
		t.Fatal("two clients for one name were accepted")
	}
	if _, err := New(Config{Name: "tenant", AppID: "3", PrivateKey: pemBytes}); err == nil {
		t.Fatal("an unknown app name was accepted")
	}
}

// The Lands App's permission set is the documented list and nothing the fleet
// holds beyond it; gardenio/scripts/github-app-register.py MANIFESTS["lands"]
// states the same set (its test checks that side).
func TestLandsTokenPermissions(t *testing.T) {
	want := map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "checks": "read", "metadata": "read"}
	if got := LandsTokenPermissions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lands permissions = %v, want %v", got, want)
	}
	for _, k := range []string{"actions", "workflows", "packages", "administration"} {
		if _, ok := LandsTokenPermissions()[k]; ok {
			t.Errorf("lands permissions include %s", k)
		}
	}
	if !reflect.DeepEqual(TokenPermissions(Fleet), AgentTokenPermissions()) || TokenPermissions("x") != nil {
		t.Fatal("TokenPermissions does not pick by name")
	}
}

// A Lands client mints with the Lands set; the fleet's stays as it was.
func TestMintUsesTheAppsPermissionSet(t *testing.T) {
	k, pemBytes := genKey(t)
	f := newFakeGitHub(t, &k.PublicKey)
	lands, err := New(Config{Name: Lands, AppID: "2", PrivateKey: pemBytes, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lands.MintInstallationToken(context.Background(), 7, []string{"garden"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.mintBodies[len(f.mintBodies)-1]["permissions"].(map[string]any)
	if len(got) != len(LandsTokenPermissions()) || got["contents"] != "write" || got["actions"] != nil {
		t.Fatalf("lands mint asked for %v", got)
	}
}

func TestConfigFromEnv(t *testing.T) {
	_, pemBytes := genKey(t)
	files := map[string][]byte{"/k": pemBytes, "/s": []byte("secret\n")}
	read := func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	full := map[string]string{
		"L_ID": "42", "L_PRIVATE_KEY_PATH": "/k", "L_CLIENT_ID": "Iv1.x", "L_CLIENT_SECRET_PATH": "/s", "L_SLUG": "gardenio-lands",
	}

	cfg, ok, err := ConfigFromEnv(Lands, "L", true, env(full), read)
	if err != nil || !ok {
		t.Fatalf("full config: ok=%v err=%v", ok, err)
	}
	if cfg.Name != Lands || cfg.AppID != "42" || cfg.Slug != "gardenio-lands" || string(cfg.ClientSecret) != "secret\n" {
		t.Fatalf("config = %+v", cfg)
	}
	c, err := New(cfg.Config)
	if err != nil || c.Name() != Lands || string(c.clientSecret) != "secret" {
		t.Fatalf("New from env config: %v", err)
	}

	// Nothing set: off, no error.
	if _, ok, err := ConfigFromEnv(Lands, "L", true, env(nil), read); ok || err != nil {
		t.Fatalf("empty env: ok=%v err=%v", ok, err)
	}
	// Any one piece missing: an error naming it, never a half App.
	for k := range full {
		part := map[string]string{}
		for kk, v := range full {
			if kk != k {
				part[kk] = v
			}
		}
		_, ok, err := ConfigFromEnv(Lands, "L", true, env(part), read)
		if ok || err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("missing %s: ok=%v err=%v", k, ok, err)
		}
	}
	// Unreadable key or secret files.
	for _, k := range []string{"L_PRIVATE_KEY_PATH", "L_CLIENT_SECRET_PATH"} {
		bad := map[string]string{}
		for kk, v := range full {
			bad[kk] = v
		}
		bad[k] = "/missing"
		if _, ok, err := ConfigFromEnv(Lands, "L", true, env(bad), read); ok || err == nil {
			t.Errorf("unreadable %s: ok=%v err=%v", k, ok, err)
		}
	}
	// Without needOAuth, id and key are enough.
	if _, ok, err := ConfigFromEnv(Fleet, "L", false, env(map[string]string{"L_ID": "1", "L_PRIVATE_KEY_PATH": "/k"}), read); !ok || err != nil {
		t.Fatalf("fleet-shaped config: ok=%v err=%v", ok, err)
	}
}
