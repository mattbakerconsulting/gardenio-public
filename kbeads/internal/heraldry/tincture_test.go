package heraldry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPalette keeps the old colour words readable: Read drops the one that
// leads a value stored in the shield form.
func TestPalette(t *testing.T) {
	want := []string{"blue", "red", "green", "gold", "purple", "brown", "slate", "sand"}
	if len(palette) != len(want) {
		t.Fatalf("palette has %d tinctures, want the eight of spec Q13", len(palette))
	}
	hex := regexp.MustCompile(`^#[0-9a-f]{6}$`)
	for i, tc := range palette {
		if tc.Name != want[i] {
			t.Errorf("tincture %d is %q, want %q (kbeadssrc and the controller mirror the order)", i, tc.Name, want[i])
		}
		if !hex.MatchString(tc.Day) || !hex.MatchString(tc.Night) {
			t.Errorf("%s: day %q and night %q must be #rrggbb", tc.Name, tc.Day, tc.Night)
		}
		if got, ok := LookupTincture(strings.ToUpper(tc.Name)); !ok || got != tc {
			t.Errorf("LookupTincture(%q) = %v, %t", strings.ToUpper(tc.Name), got, ok)
		}
		if _, ok := Lookup(tc.Name); ok || tc.Name == "on" || tc.Name == "none" {
			t.Errorf("%q cannot be a colour word", tc.Name)
		}
	}
}

// TestMigration058ColourIsATincture keeps the rows migration 058 coloured
// readable: the colour it put before rows stored with no field colour is a
// palette word, which Read drops.
func TestMigration058ColourIsATincture(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "store", "postgres", "migrations", "058_beads_heraldry_colour.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`SET heraldry = '([a-z]+) ' \|\| heraldry`).FindSubmatch(src)
	if m == nil {
		t.Fatal("migration 058 gives no colour")
	}
	if _, ok := LookupTincture(string(m[1])); !ok {
		t.Errorf("migration 058 gives %s, which is not a palette tincture", m[1])
	}
}
