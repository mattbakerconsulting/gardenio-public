package landtoken

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vector was computed outside Go (Python hmac + urlsafe_b64encode), so a
// drift from coopmux's format in either the message or the encoding fails.
func TestMint_Vector(t *testing.T) {
	got, err := Mint([]byte("test-key"), "acme")
	if err != nil {
		t.Fatal(err)
	}
	const want = "coopmux-land.v1.acme.V1-tj4qwZnytTzFp_lm8Wh1xSDNEI2ysk6vWP_xj3TI"
	if got != want {
		t.Errorf("Mint = %q, want %q", got, want)
	}
}

func TestMint_RefusesWhatCoopmuxRefuses(t *testing.T) {
	for _, land := range []string{"", "Acme", "a.b", "a_b", "acme/x", string(make([]byte, 59))} {
		if _, err := Mint([]byte("k"), land); !errors.Is(err, ErrNotALand) {
			t.Errorf("Mint(%q) err = %v, want ErrNotALand", land, err)
		}
	}
	if _, err := Mint(nil, "acme"); err == nil {
		t.Error("Mint with an empty key succeeded")
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	if err := os.WriteFile(good, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadKey(good)
	if err != nil || string(key) != "test-key" {
		t.Fatalf("LoadKey = %q, %v; want the trimmed key", key, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(empty); err == nil {
		t.Error("LoadKey of a blank file succeeded")
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("LoadKey of a missing file succeeded")
	}
}

func TestMintNATS_VerifyRoundTrip(t *testing.T) {
	key := []byte("test-key")
	tok, err := MintNATS(key, "acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNATS(key, tok, "acme", 0); err != nil {
		t.Fatalf("VerifyNATS(MintNATS) = %v", err)
	}
	coopmux, _ := Mint(key, "acme")
	if coopmux == tok {
		t.Fatal("the NATS token equals the coopmux token")
	}
	other, _ := MintNATS(key, "beta", 0)
	for _, bad := range []string{
		"", coopmux, tok + "x", "nats-land.v1.acme", "nats-land.v1.Acme." + tok[len("nats-land.v1.acme."):],
		// another land's MAC under this land's name
		"nats-land.v1.acme." + other[len("nats-land.v1.beta."):],
		other,
	} {
		if err := VerifyNATS(key, bad, "acme", 0); err == nil {
			t.Errorf("VerifyNATS(%q) accepted", bad)
		}
	}
	if err := VerifyNATS([]byte("other-key"), tok, "acme", 0); err == nil {
		t.Error("VerifyNATS under another key accepted")
	}
}

// Bumping a land's epoch revokes the passwords minted at every other epoch,
// for that land alone (gd-zo4W7Q8Yno).
func TestMintNATS_EpochRevokes(t *testing.T) {
	key := []byte("test-key")
	at := func(land string, epoch uint64) string {
		t.Helper()
		tok, err := MintNATS(key, land, epoch)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if got, want := at("acme", 0), "nats-land.v1.acme."; !strings.HasPrefix(got, want) || strings.Count(got, ".") != 3 {
		t.Errorf("epoch 0 password = %q, want the pre-epoch shape %s<mac>", got, want)
	}
	if got := at("acme", 2); !strings.HasPrefix(got, "nats-land.v1.acme.2.") {
		t.Errorf("epoch 2 password = %q, want prefix nats-land.v1.acme.2.", got)
	}
	for _, old := range []uint64{0, 1, 3} {
		if err := VerifyNATS(key, at("acme", old), "acme", 2); err == nil {
			t.Errorf("epoch %d password verified at epoch 2", old)
		}
	}
	if err := VerifyNATS(key, at("acme", 2), "acme", 2); err != nil {
		t.Errorf("epoch 2 password at epoch 2: %v", err)
	}
	if err := VerifyNATS(key, at("beta", 0), "beta", 0); err != nil {
		t.Errorf("another land's password broke with acme's bump: %v", err)
	}
	if err := VerifyNATS(key, at("beta", 2), "acme", 2); err == nil {
		t.Error("land beta's epoch 2 password verified as acme's")
	}
}
