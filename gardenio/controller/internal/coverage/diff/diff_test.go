package diff

import (
	"reflect"
	"strings"
	"testing"
)

const sample = `diff --git a/gardenio/controller/internal/foo/bar.go b/gardenio/controller/internal/foo/bar.go
index abc..def 100644
--- a/gardenio/controller/internal/foo/bar.go
+++ b/gardenio/controller/internal/foo/bar.go
@@ -10,0 +11,3 @@ func Foo() {
+	a := 1
+	b := 2
+	return a + b
@@ -30,2 +33,1 @@ func Bar() {
-	old1
-	old2
+	new
diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,5 +0,0 @@
-line1
-line2
diff --git a/kbeads/internal/store/store.go b/kbeads/internal/store/store.go
--- a/kbeads/internal/store/store.go
+++ b/kbeads/internal/store/store.go
@@ -5,0 +6 @@
+single
`

func TestParse(t *testing.T) {
	d, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(d.Files); got != 2 {
		t.Fatalf("expected 2 files (deletion dropped), got %d", got)
	}
	bar := d.Files["gardenio/controller/internal/foo/bar.go"]
	if bar == nil {
		t.Fatal("missing bar.go")
	}
	want := []int{11, 12, 13, 33}
	if !reflect.DeepEqual(bar.AddedLines, want) {
		t.Errorf("bar.go AddedLines = %v; want %v", bar.AddedLines, want)
	}
	store := d.Files["kbeads/internal/store/store.go"]
	if store == nil {
		t.Fatal("missing store.go")
	}
	if !reflect.DeepEqual(store.AddedLines, []int{6}) {
		t.Errorf("store.go AddedLines = %v; want [6]", store.AddedLines)
	}
}

func TestParseEmpty(t *testing.T) {
	d, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(d.Files) != 0 {
		t.Errorf("empty input should yield 0 files, got %d", len(d.Files))
	}
}

func TestParseHunkNewLine(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"@@ -10,0 +11,3 @@", 11, true},
		{"@@ -30,2 +33,1 @@ ctx", 33, true},
		{"@@ -5,0 +6 @@", 6, true},
		{"@@ -1 +1 @@", 1, true},
		{"@@ malformed @@", 0, false},
	}
	for _, c := range cases {
		got, ok := parseHunkNewLine(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseHunkNewLine(%q) = (%d,%v); want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
