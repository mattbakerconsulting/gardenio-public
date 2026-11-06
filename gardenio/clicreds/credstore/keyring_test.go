package credstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// withMockKeyring swaps the go-keyring provider for the in-memory mock for
// the duration of the test. The mock is process-global; do not run keyring
// tests in parallel.
func withMockKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(func() {
		// Best-effort reset between tests so a stale "gardenio" service
		// entry can't leak across cases.
		_ = keyring.DeleteAll(keyringService)
	})
}

func newTestKeyringBackend(t *testing.T) (*keyringBackend, string) {
	t.Helper()
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	migrateFrom := filepath.Join(dir, "credentials.json")
	return newKeyringBackend(indexPath, migrateFrom), dir
}

func TestKeyringBackend_SaveAndLoad(t *testing.T) {
	withMockKeyring(t)
	be, dir := newTestKeyringBackend(t)

	in := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {
				Server:      "https://beads.example.com",
				AccessToken: "at-abc.xyz",
				User:        &User{Email: "matt@example.com", Username: "matt"},
			},
		},
	}
	if err := be.save(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Index file must exist with 0600 perms and contain only the
	// non-sensitive name → server mapping (no token material).
	indexPath := filepath.Join(dir, "index.json")
	info, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat index: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("index perm = %04o, want 0600", perm)
	}
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); strings.Contains(got, "abc") || strings.Contains(got, "xyz") {
		t.Errorf("index file leaked token material: %s", got)
	}

	// Keyring entry must be present, keyed by daemon URL.
	stored, err := keyring.Get(keyringService, "https://beads.example.com")
	if err != nil {
		t.Fatalf("keyring get: %v", err)
	}
	decoded, err := decodeKeyringValue(stored)
	if err != nil {
		t.Fatalf("decode keyring entry: %v", err)
	}
	var roundtrip Context
	if err := json.Unmarshal(decoded, &roundtrip); err != nil {
		t.Fatalf("unmarshal keyring entry: %v", err)
	}
	if roundtrip.AccessToken != "at-abc.xyz" {
		t.Errorf("token roundtrip = %q", roundtrip.AccessToken)
	}

	// Load back through the backend.
	out, err := be.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.CurrentContext != "default" {
		t.Errorf("current = %q", out.CurrentContext)
	}
	cur := out.Contexts["default"]
	if cur == nil {
		t.Fatal("default context missing after reload")
	}
	if cur.AccessToken != "at-abc.xyz" {
		t.Errorf("loaded token = %q", cur.AccessToken)
	}
	if cur.User.Username != "matt" {
		t.Errorf("loaded username = %q", cur.User.Username)
	}
}

func TestKeyringBackend_KeyedByDaemonURL(t *testing.T) {
	withMockKeyring(t)
	be, _ := newTestKeyringBackend(t)

	in := storeData{
		CurrentContext: "prod",
		Contexts: map[string]*Context{
			"dev":  {Server: "http://localhost:8080", AccessToken: "dev-token"},
			"prod": {Server: "https://prod.example.com", AccessToken: "prod-token"},
		},
	}
	if err := be.save(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Each daemon URL gets its own keyring entry.
	dev, err := keyring.Get(keyringService, "http://localhost:8080")
	if err != nil {
		t.Fatalf("dev get: %v", err)
	}
	prod, err := keyring.Get(keyringService, "https://prod.example.com")
	if err != nil {
		t.Fatalf("prod get: %v", err)
	}
	if dev == prod {
		t.Error("dev and prod keyring entries should not collide")
	}
}

func TestKeyringBackend_DeleteContextRemovesKeyringEntry(t *testing.T) {
	withMockKeyring(t)
	be, _ := newTestKeyringBackend(t)

	first := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"dev":  {Server: "http://localhost:8080", AccessToken: "dev"},
			"prod": {Server: "https://prod.example.com", AccessToken: "prod"},
		},
	}
	if err := be.save(first); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Drop prod, save again, then check the prod keyring entry is gone.
	second := storeData{
		CurrentContext: "dev",
		Contexts: map[string]*Context{
			"dev": {Server: "http://localhost:8080", AccessToken: "dev"},
		},
	}
	if err := be.save(second); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if _, err := keyring.Get(keyringService, "https://prod.example.com"); err == nil {
		t.Error("expected prod keyring entry to be deleted")
	}
	if _, err := keyring.Get(keyringService, "http://localhost:8080"); err != nil {
		t.Errorf("expected dev keyring entry to survive: %v", err)
	}
}

func TestKeyringBackend_AtomicTokenRefresh(t *testing.T) {
	// "Atomic update on token refresh" — a save that updates the AccessToken
	// field must replace the keyring entry in one shot, not leave a
	// half-written intermediate.
	withMockKeyring(t)
	be, _ := newTestKeyringBackend(t)

	initial := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {Server: "https://beads.example.com", AccessToken: "old"},
		},
	}
	if err := be.save(initial); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	// Mutate the token in place and save (the token-refresh pattern).
	updated := initial
	updated.Contexts["default"].AccessToken = "new"
	if err := be.save(updated); err != nil {
		t.Fatalf("refresh save: %v", err)
	}

	raw, err := keyring.Get(keyringService, "https://beads.example.com")
	if err != nil {
		t.Fatalf("keyring get: %v", err)
	}
	decoded, err := decodeKeyringValue(raw)
	if err != nil {
		t.Fatalf("decode keyring entry: %v", err)
	}
	var c Context
	if err := json.Unmarshal(decoded, &c); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "new" {
		t.Errorf("post-refresh token = %q, want new", c.AccessToken)
	}
}

func TestKeyringBackend_MigratesLegacyFile(t *testing.T) {
	withMockKeyring(t)
	be, dir := newTestKeyringBackend(t)
	legacy := filepath.Join(dir, "credentials.json")

	// Seed a legacy credentials.json with tokens.
	legacyData := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {
				Server:      "https://beads.example.com",
				AccessToken: "legacy-at",
				IDToken:     "legacy-id",
				ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
				User:        &User{Email: "matt@example.com", Username: "matt"},
			},
		},
	}
	if err := writeJSONAtomic(legacy, legacyData); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	out, err := be.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur := out.Contexts["default"]
	if cur == nil || cur.AccessToken != "legacy-at" {
		t.Fatalf("migration didn't preserve token: %+v", cur)
	}

	// Original file must be renamed (so the keyring is the source of
	// truth from now on).
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file should be renamed, got err=%v", err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Errorf("expected .migrated sidecar: %v", err)
	}

	// Keyring should now hold the migrated entry.
	if _, err := keyring.Get(keyringService, "https://beads.example.com"); err != nil {
		t.Errorf("expected keyring entry after migration: %v", err)
	}

	// Second load must NOT re-migrate (legacy file is gone) and must
	// still return the data from the keyring.
	out2, err := be.load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got := out2.Contexts["default"].AccessToken; got != "legacy-at" {
		t.Errorf("post-migration load token = %q", got)
	}
}

// makeLargeToken returns a JWT-shaped token of roughly 2n bytes (a large
// id_token carrying many group claims) so the serialized context crosses the
// macOS Keychain ~4KB SecItemAdd cap.
func makeLargeToken(n int) string {
	return "eyJhbGciOiJSUzI1NiJ9." + strings.Repeat("a1B2c3D4e5F6", (n/12)+1) +
		"." + strings.Repeat("Z9y8X7w6V5u4", (n/12)+1)
}

func TestKeyringBackend_LargeCredentialRoundTrip(t *testing.T) {
	// A >4KB credential must survive save()/load() — gzip keeps the stored
	// keyring entry under the macOS Keychain cap that previously rejected it
	// with "data passed to Set was too big" (ka-0000008122).
	withMockKeyring(t)
	be, _ := newTestKeyringBackend(t)

	bigToken := makeLargeToken(6000)
	in := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {
				Server:  "https://beads.example.com",
				IDToken: bigToken,
				User:    &User{Email: "matt@example.com", Username: "matt"},
			},
		},
	}

	// Sanity: the uncompressed JSON must actually exceed the ~4KB cap so the
	// test is exercising the regression, not a trivially-small payload.
	rawJSON, err := json.Marshal(in.Contexts["default"])
	if err != nil {
		t.Fatal(err)
	}
	if len(rawJSON) <= 4096 {
		t.Fatalf("test payload too small (%d bytes) — must exceed 4KB to cover the regression", len(rawJSON))
	}

	if err := be.save(in); err != nil {
		t.Fatalf("save large credential: %v", err)
	}

	// The stored keyring value must be gzip-compressed and well under 4KB.
	stored, err := keyring.Get(keyringService, "https://beads.example.com")
	if err != nil {
		t.Fatalf("keyring get: %v", err)
	}
	if len(stored) >= 4096 {
		t.Errorf("stored keyring entry = %d bytes, want < 4096 (gzip should shrink it)", len(stored))
	}
	if !strings.HasPrefix(stored, string(gzipMagic)) {
		t.Errorf("stored keyring entry is not gzip-compressed (missing magic prefix)")
	}

	out, err := be.load()
	if err != nil {
		t.Fatalf("load large credential: %v", err)
	}
	cur := out.Contexts["default"]
	if cur == nil {
		t.Fatal("default context missing after reload")
	}
	if cur.IDToken != bigToken {
		t.Errorf("large token did not round-trip (got %d bytes, want %d)", len(cur.IDToken), len(bigToken))
	}
	if cur.User == nil || cur.User.Username != "matt" {
		t.Errorf("user metadata lost on large round-trip: %+v", cur.User)
	}
}

func TestKeyringBackend_MigratesLargeLegacyFile(t *testing.T) {
	// The reported symptom: a legacy credentials.json >4KB fails migration
	// with "data passed to Set was too big". After the gzip fix, migration
	// must succeed (ka-0000008122).
	withMockKeyring(t)
	be, dir := newTestKeyringBackend(t)
	legacy := filepath.Join(dir, "credentials.json")

	bigToken := makeLargeToken(6000)
	legacyData := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {
				Server:  "https://beads.gardenio.app.e2e.dev.acme.example",
				IDToken: bigToken,
				User:    &User{Email: "matt@example.com", Username: "matt"},
			},
		},
	}
	if err := writeJSONAtomic(legacy, legacyData); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	out, err := be.load()
	if err != nil {
		t.Fatalf("migrate large legacy file: %v", err)
	}
	cur := out.Contexts["default"]
	if cur == nil || cur.IDToken != bigToken {
		t.Fatalf("migration didn't preserve large token: %+v", cur)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Errorf("expected .migrated sidecar after large migration: %v", err)
	}
}

func TestKeyringBackend_LoadsLegacyPlaintextEntry(t *testing.T) {
	// Backward compatibility: keyring entries written before the gzip change
	// are stored as plaintext JSON. load() must still decode them (magic-byte
	// sniff returns plaintext unchanged) — ka-0000008122.
	withMockKeyring(t)
	be, _ := newTestKeyringBackend(t)

	// Write a plaintext (un-gzipped) entry directly, mimicking the pre-fix
	// on-disk format, plus the matching index.
	plain := &Context{
		Server:      "https://legacy.example.com",
		AccessToken: "plaintext-token",
		User:        &User{Email: "old@example.com", Username: "old"},
	}
	rawJSON, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Set(keyringService, plain.Server, string(rawJSON)); err != nil {
		t.Fatalf("seed plaintext keyring entry: %v", err)
	}
	idx := keyringIndex{
		CurrentContext: "default",
		Contexts:       map[string]string{"default": plain.Server},
	}
	if err := writeJSONAtomic(be.indexPath, idx); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	out, err := be.load()
	if err != nil {
		t.Fatalf("load legacy plaintext: %v", err)
	}
	cur := out.Contexts["default"]
	if cur == nil || cur.AccessToken != "plaintext-token" {
		t.Fatalf("legacy plaintext entry did not load: %+v", cur)
	}
	if cur.User == nil || cur.User.Username != "old" {
		t.Errorf("legacy plaintext user metadata lost: %+v", cur.User)
	}
}

func TestKeyringBackend_IndexDriftSkipsMissingEntry(t *testing.T) {
	withMockKeyring(t)
	be, _ := newTestKeyringBackend(t)

	in := storeData{
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {Server: "https://beads.example.com", AccessToken: "c"},
			"orphan":  {Server: "https://orphan.example.com", AccessToken: "o"},
		},
	}
	if err := be.save(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Simulate an out-of-band deletion (e.g. operator wipes the
	// Keychain entry by hand).
	if err := keyring.Delete(keyringService, "https://orphan.example.com"); err != nil {
		t.Fatalf("force-delete: %v", err)
	}

	out, err := be.load()
	if err != nil {
		t.Fatalf("load after drift: %v", err)
	}
	if _, ok := out.Contexts["orphan"]; ok {
		t.Error("expected orphan to be skipped after keyring drift")
	}
	if _, ok := out.Contexts["default"]; !ok {
		t.Error("expected default to survive")
	}
}

func TestLoad_ForcesFileBackendWhenEnvSet(t *testing.T) {
	t.Setenv("GB_CREDSTORE_BACKEND", "file")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetContext("default", &Context{Server: "https://x", AccessToken: "c"})
	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File at DefaultPath() must exist; describe() must say "file …".
	if _, err := os.Stat(DefaultPath()); err != nil {
		t.Errorf("expected default credentials.json: %v", err)
	}
	if got := s.Describe(); !strings.Contains(got, "file ") {
		t.Errorf("Describe = %q, want file prefix", got)
	}
}

func TestLoad_ForcesKeyringBackendWhenEnvSet(t *testing.T) {
	withMockKeyring(t)
	t.Setenv("GB_CREDSTORE_BACKEND", "keyring")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetContext("default", &Context{Server: "https://x", AccessToken: "c"})
	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Keyring entry must exist; describe() must mention OS secret store.
	if _, err := keyring.Get(keyringService, "https://x"); err != nil {
		t.Errorf("expected keyring entry: %v", err)
	}
	if got := s.Describe(); !strings.Contains(got, "OS secret store") {
		t.Errorf("Describe = %q", got)
	}
}

func TestLoad_InvalidBackendEnv(t *testing.T) {
	t.Setenv("GB_CREDSTORE_BACKEND", "lolwut")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error for invalid GB_CREDSTORE_BACKEND")
	}
}

func TestLoad_ExplicitPathUsesFileBackend(t *testing.T) {
	withMockKeyring(t) // would otherwise be auto-selected
	path := filepath.Join(t.TempDir(), "creds.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.Describe(); got != "file "+path {
		t.Errorf("Describe = %q, want file %s", got, path)
	}
}
