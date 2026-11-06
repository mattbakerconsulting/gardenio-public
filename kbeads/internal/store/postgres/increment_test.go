package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/store"
)

// TestQueryIncrementBeadFields_RejectsSQLInjectionKey is the regression test
// for AUDIT-2026-04-18 §C1. Before the fix, queryIncrementBeadFields
// Sprintf-interpolated caller-supplied JSON keys directly into SQL, allowing
// arbitrary SQL injection via POST /v1/beads/{id}/increment.
func TestQueryIncrementBeadFields_RejectsSQLInjectionKey(t *testing.T) {
	db, _ := newMockDB(t)

	injections := []string{
		`x')); DROP TABLE beads; --`,
		`x','}',(SELECT 1); --`,
		`}', (SELECT current_user), '{x`,
		`x\",\"y`,
		`x'y`,
		`x y`,
		"\tabc",
		"",
	}

	for _, key := range injections {
		t.Run(key, func(t *testing.T) {
			_, err := queryIncrementBeadFields(context.Background(), db, "gd-test", map[string]float64{key: 1})
			if !errors.Is(err, store.ErrInvalidFieldKey) {
				t.Fatalf("expected ErrInvalidFieldKey for key %q, got %v", key, err)
			}
		})
	}
	// No SQL expectations set — if validation fails to short-circuit, sqlmock
	// will flag the unexpected QueryRowContext in cleanup.
}

// TestQueryIncrementBeadFields_RejectsBatchIfAnyKeyInvalid ensures a single
// bad key in a multi-key batch poisons the whole request — the fix must not
// silently drop the bad one and process the rest.
func TestQueryIncrementBeadFields_RejectsBatchIfAnyKeyInvalid(t *testing.T) {
	db, _ := newMockDB(t)

	deltas := map[string]float64{
		"good_key":         1,
		"x'); DROP TABLE;": 2,
	}
	_, err := queryIncrementBeadFields(context.Background(), db, "gd-test", deltas)
	if !errors.Is(err, store.ErrInvalidFieldKey) {
		t.Fatalf("expected ErrInvalidFieldKey, got %v", err)
	}
	// The key is included in the error message so operators can see which
	// field was rejected.
	if !strings.Contains(err.Error(), "DROP TABLE") {
		t.Errorf("expected error to name the offending key; got %v", err)
	}
}

func TestValidFieldKey(t *testing.T) {
	valid := []string{"count", "retry_count", "_private", "a", "A1", "foo_bar_baz"}
	for _, k := range valid {
		if !store.ValidFieldKey(k) {
			t.Errorf("expected %q to be valid", k)
		}
	}
	invalid := []string{
		"", "1leading", "has space", "has-dash", "has.dot", "has'quote",
		`has"doublequote`, "has{brace}", "nul\x00byte", "unicode\u00e9",
	}
	for _, k := range invalid {
		if store.ValidFieldKey(k) {
			t.Errorf("expected %q to be invalid", k)
		}
	}
}
