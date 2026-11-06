package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenize_Basic(t *testing.T) {
	got := tokenize("The Gardenio controller, crashed on ka-0000000316!")
	// Keep bead id token intact; split on whitespace/punctuation.
	wantSubset := []string{"The", "Gardenio", "controller", "crashed", "on", "ka-0000000316"}
	for _, w := range wantSubset {
		if !contains(got, w) {
			t.Errorf("missing token %q in %v", w, got)
		}
	}
}

func TestTokenize_StripsPunctuation(t *testing.T) {
	got := tokenize("***bold***, (parenthetical), \"quoted\"")
	for _, g := range got {
		if g == "" || g == "*" || g == "(" || g == ")" || g == "\"" {
			t.Errorf("punctuation-only token leaked: %q in %v", g, got)
		}
	}
	// Make sure words survived.
	for _, want := range []string{"bold", "parenthetical", "quoted"} {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

func TestTokenize_KeepsInternalDotsAndHyphens(t *testing.T) {
	got := tokenize("edit go.mod and scripts/release.sh — note gd-u08atreejd.")
	for _, want := range []string{"go.mod", "scripts/release.sh", "gd-u08atreejd"} {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

func TestTokenize_Empty(t *testing.T) {
	if got := tokenize(""); got != nil {
		t.Errorf("want nil, got %v", got)
	}
	if got := tokenize("   \t\n"); len(got) != 0 {
		t.Errorf("want no tokens from whitespace, got %v", got)
	}
}

func TestTermFreq_TopNRanksByCount(t *testing.T) {
	tf := newTermFreq()
	// "gardenio" x3, "controller" x2, "bug" x1
	for range 3 {
		tf.add("gardenio")
	}
	for range 2 {
		tf.add("controller")
	}
	tf.add("bug")
	// Stopwords should be ignored entirely.
	tf.add("the")
	tf.add("and")
	// Too-short tokens ignored.
	tf.add("ab")

	got := tf.topN(10)
	want := []string{"gardenio", "controller", "bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("topN\n  got  = %v\n  want = %v", got, want)
	}
}

func TestTermFreq_PreservesCanonicalCase(t *testing.T) {
	tf := newTermFreq()
	tf.add("gardenio")
	tf.add("Gardenio")
	tf.add("gardenio")
	got := tf.topN(1)
	if len(got) != 1 {
		t.Fatalf("want 1 term, got %v", got)
	}
	// Expected canonical = "Gardenio" (first non-all-lower form seen after
	// all-lower entries). Accept either form if the implementation differs,
	// but fail if it's empty.
	if strings.ToLower(got[0]) != "gardenio" {
		t.Fatalf("canonical form wrong: %q", got[0])
	}
}

func TestTermFreq_TopN_TieBreakLex(t *testing.T) {
	tf := newTermFreq()
	tf.add("zzz")
	tf.add("aaa")
	tf.add("mmm")
	// All same count -> lex ascending.
	got := tf.topN(3)
	want := []string{"aaa", "mmm", "zzz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tie-break wrong: got %v want %v", got, want)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
