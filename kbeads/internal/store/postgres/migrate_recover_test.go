package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestHandleMigrateError_NonDirty verifies non-ErrDirty errors are wrapped
// with the generic "apply migrations" prefix, preserving the underlying
// cause for errors.Is/As.
func TestHandleMigrateError_NonDirty(t *testing.T) {
	underlying := errors.New("connection refused")
	got := handleMigrateError(nil, underlying)
	if got == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(got, underlying) {
		t.Errorf("expected wrapped error to unwrap to underlying; got %v", got)
	}
	if !strings.Contains(got.Error(), "apply migrations") {
		t.Errorf("expected 'apply migrations' prefix; got %q", got.Error())
	}
}

// TestHandleMigrateError_DirtyNoAutoRecover verifies the default behavior
// (no env var): return an actionable error with the last-clean version and
// the BEADS_MIGRATE_AUTO_RECOVER hint, and do not call Force/Up. Uses nil
// migrator since this path must not dereference it.
func TestHandleMigrateError_DirtyNoAutoRecover(t *testing.T) {
	t.Setenv("BEADS_MIGRATE_AUTO_RECOVER", "")
	dirty := migrate.ErrDirty{Version: 5}

	got := handleMigrateError(nil, dirty)
	if got == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(got, dirty) {
		t.Errorf("expected wrapped error to unwrap to ErrDirty; got %v", got)
	}
	msg := got.Error()
	for _, want := range []string{
		"BEADS_MIGRATE_AUTO_RECOVER=last-clean",
		"version 4", // last-clean = dirty_version - 1
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to contain %q; got %q", want, msg)
		}
	}
}

// TestHandleMigrateError_DirtyAutoRecoverButNilMigrator verifies the
// auto-recover branch is taken when the env var is set. We can't invoke
// Force/Up without a real migrator, but the function must attempt to — if
// it returns the same "re-run with BEADS_MIGRATE_AUTO_RECOVER" hint it would
// return when disabled, the branch logic is wrong. With a nil migrator the
// Force call panics; we assert it panics rather than returning the disabled
// message, which proves the env-var branch was taken.
func TestHandleMigrateError_DirtyAutoRecoverTakesBranch(t *testing.T) {
	t.Setenv("BEADS_MIGRATE_AUTO_RECOVER", "last-clean")
	dirty := migrate.ErrDirty{Version: 5}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil migrator (proves Force branch was entered); got clean return")
		}
	}()
	_ = handleMigrateError(nil, dirty)
	t.Fatal("unreachable — handleMigrateError with nil migrator + auto-recover must panic before returning")
}

// Compile-time assertion that ErrDirty's Error message format (which we
// parse indirectly via errors.As) is what we expect. If upstream changes
// the struct, this test fails loudly.
func TestErrDirty_Shape(t *testing.T) {
	d := migrate.ErrDirty{Version: 42}
	want := fmt.Sprintf("Dirty database version %d. Fix and force version.", 42)
	if d.Error() != want {
		t.Errorf("ErrDirty shape changed upstream: got %q want %q", d.Error(), want)
	}
}
