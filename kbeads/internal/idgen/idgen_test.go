package idgen

import (
	"regexp"
	"testing"
)

func TestGenerate_Length(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	wantLen := len(DefaultPrefix) + Length
	if len(id) != wantLen {
		t.Errorf("Generate() length = %d, want %d (id=%q)", len(id), wantLen, id)
	}
}

func TestGenerate_DefaultPrefix(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if id[:len(DefaultPrefix)] != DefaultPrefix {
		t.Errorf("Generate() = %q, want prefix %q", id, DefaultPrefix)
	}
}

func TestGenerate_Charset(t *testing.T) {
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(DefaultPrefix) + `[a-zA-Z0-9]+$`)
	for i := 0; i < 100; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error on iteration %d: %v", i, err)
		}
		if !pattern.MatchString(id) {
			t.Fatalf("Generate() = %q, does not match expected charset pattern", id)
		}
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	const count = 10_000
	seen := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error on iteration %d: %v", i, err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID after %d generations: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestGenerateWithPrefix(t *testing.T) {
	prefix := "test-"
	id, err := GenerateWithPrefix(prefix)
	if err != nil {
		t.Fatalf("GenerateWithPrefix(%q) error: %v", prefix, err)
	}

	if id[:len(prefix)] != prefix {
		t.Errorf("GenerateWithPrefix(%q) = %q, want prefix %q", prefix, id, prefix)
	}

	wantLen := len(prefix) + Length
	if len(id) != wantLen {
		t.Errorf("GenerateWithPrefix(%q) length = %d, want %d (id=%q)", prefix, len(id), wantLen, id)
	}

	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `[a-zA-Z0-9]+$`)
	if !pattern.MatchString(id) {
		t.Errorf("GenerateWithPrefix(%q) = %q, does not match expected charset pattern", prefix, id)
	}
}
