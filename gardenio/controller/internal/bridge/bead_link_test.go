package bridge

import "testing"

func TestBeadLinkEmptyViewerURLReturnsPlainLabel(t *testing.T) {
	b := &Bot{beadsViewerURL: ""}
	got := b.beadLink("gd-abc", "My bead")
	if got != "My bead" {
		t.Fatalf("expected plain label, got %q", got)
	}
}

func TestBeadLinkHappyPath(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://viewer"}
	got := b.beadLink("gd-abc", "My bead")
	want := "<https://viewer/gd-abc|My bead>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBeadLinkRejectsNonWhitelistedID(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://viewer"}
	cases := []string{"../evil", "gd-abc/../foo", "", "not-a-bead", "gd-", "gd-abc?query=1"}
	for _, id := range cases {
		got := b.beadLink(id, "Label")
		if got != "Label" {
			t.Errorf("id=%q: expected plain label fallback, got %q", id, got)
		}
	}
}

func TestBeadLinkEscapesLabel(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://viewer"}
	got := b.beadLink("gd-abc", "<script>&")
	want := "<https://viewer/gd-abc|&lt;script&gt;&amp;>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBeadLinkTrimsTrailingSlash(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://viewer/"}
	got := b.beadLink("gd-abc", "L")
	want := "<https://viewer/gd-abc|L>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBeadLinkEscapesLabelEvenWhenPlain(t *testing.T) {
	b := &Bot{beadsViewerURL: ""}
	got := b.beadLink("gd-abc", "<script>&")
	want := "&lt;script&gt;&amp;"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBeadTitleLinkUsesTitle(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://viewer"}
	got := b.beadTitleLink("gd-abc", "Nice title")
	want := "<https://viewer/gd-abc|Nice title>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBeadTitleLinkEmptyTitleFallsBackToID(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://viewer"}
	got := b.beadTitleLink("gd-abc", "")
	want := "<https://viewer/gd-abc|gd-abc>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBeadTitleLinkEmptyViewerURLFallsBackToPlain(t *testing.T) {
	b := &Bot{beadsViewerURL: ""}
	got := b.beadTitleLink("gd-abc", "")
	if got != "gd-abc" {
		t.Fatalf("expected plain id, got %q", got)
	}
}
