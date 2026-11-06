package lcov

import (
	"strings"
	"testing"
)

const sample = `TN:
SF:gardenio/controller/internal/foo/bar.go
DA:10,1
DA:11,0
DA:12,3
LH:2
LF:3
end_of_record
SF:kbeads/internal/store/store.go
DA:5,2
DA:7,0,abcdef
end_of_record
SF:gardenio/controller/internal/foo/bar.go
DA:11,4
DA:13,1
end_of_record
`

func TestParse(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(f.Records); got != 2 {
		t.Fatalf("expected 2 unique SFs, got %d", got)
	}
	bar := f.Records["gardenio/controller/internal/foo/bar.go"]
	if bar == nil {
		t.Fatal("missing bar.go record")
	}
	// Same-path records merge (sum DA hits).
	if got := bar.Lines[10]; got != 1 {
		t.Errorf("bar.go line 10 hits = %d; want 1", got)
	}
	if got := bar.Lines[11]; got != 4 {
		t.Errorf("bar.go line 11 hits (merged 0+4) = %d; want 4", got)
	}
	if got := bar.Lines[12]; got != 3 {
		t.Errorf("bar.go line 12 hits = %d; want 3", got)
	}
	if got := bar.Lines[13]; got != 1 {
		t.Errorf("bar.go line 13 hits = %d; want 1", got)
	}
	store := f.Records["kbeads/internal/store/store.go"]
	if store == nil {
		t.Fatal("missing store.go record")
	}
	// Checksum-suffixed DA still parses.
	if got := store.Lines[7]; got != 0 {
		t.Errorf("store.go line 7 hits = %d; want 0", got)
	}
}

func TestParseEmpty(t *testing.T) {
	f, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.Records) != 0 {
		t.Errorf("empty input should produce no records, got %d", len(f.Records))
	}
}
