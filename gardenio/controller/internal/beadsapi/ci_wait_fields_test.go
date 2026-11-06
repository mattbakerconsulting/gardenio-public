package beadsapi

import (
	"strings"
	"testing"
)

func TestCIWaitKey(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	got, err := CIWaitKey(" joyousaversion/gardenio ", strings.ToUpper(sha))
	if err != nil {
		t.Fatal(err)
	}
	if want := "joyousaversion/gardenio@" + sha; got != want {
		t.Errorf("CIWaitKey = %q, want %q (sha case-folded, repo trimmed)", got, want)
	}

	for _, tc := range []struct{ name, repo, sha string }{
		{"abbreviated sha", "o/r", sha[:7]},
		{"41-char sha", "o/r", sha + "0"},
		{"non-hex sha", "o/r", strings.Repeat("g", 40)},
		{"bare repo name", "gardenio", sha},
		{"too many path segments", "o/r/x", sha},
		{"empty repo", "", sha},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CIWaitKey(tc.repo, tc.sha); err == nil {
				t.Errorf("CIWaitKey(%q, %q) accepted; want an error", tc.repo, tc.sha)
			}
		})
	}
}

func TestCIWaitStates(t *testing.T) {
	for _, tc := range []struct {
		state              string
		pending, concluded bool
	}{
		{"", false, false},
		{CIWaitStatePending, true, false},
		{CIWaitStateResolved, false, true},
		{CIWaitStateTimedOut, false, true},
		{CIWaitStateConsumed, false, false},
	} {
		w := CIWaitFromFields(map[string]string{FieldCIWaitState: tc.state})
		if w.Pending() != tc.pending || w.Concluded() != tc.concluded {
			t.Errorf("state %q: Pending=%v Concluded=%v, want %v/%v",
				tc.state, w.Pending(), w.Concluded(), tc.pending, tc.concluded)
		}
	}
}
