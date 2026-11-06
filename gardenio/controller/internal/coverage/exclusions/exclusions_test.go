package exclusions

import "testing"

func TestIsExcludedDefault(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"gardenio/controller/internal/foo/bar.go", false},
		{"gardenio/controller/internal/foo/bar_test.go", true},
		{"kbeads/gen/proto/foo.pb.go", true},
		{"kbeads/gen/sqlc/queries.sql.go", true},
		{"kbeads/internal/store/queries_sqlc.go", true},
		{"coop/proto/coop.proto", true},
		{"coop/crates/cli/src/main.rs", false},
		{"coop/crates/cli/tests/foo.rs", true},
		{"beads3d/src/app.test.ts", true},
		{"beads3d/src/app.spec.js", true},
		{"beads3d/src/app.ts", false},
		{"beads3d/dist/bundle.js", true},
		{"node_modules/foo/bar.js", true},
		{"vendor/golang.org/x/foo.go", true},
		{"coop/target/debug/foo.rs", true},
		{"gardenio/controller/internal/foo/testdata/golden.lcov", true},
		{"gardenio/controller/internal/foo/fixtures/x.go", true},
		// e2e-only probe binaries are excluded (ka-0000007875): they are
		// built solely by .github/workflows/e2e.yml, never ship in a prod image, and
		// MR-event coverage runs are unit-only so their patch coverage is
		// structurally 0%.
		{"gardenio/controller/cmd/jira-conv-probe/main.go", true},
		{"gardenio/controller/cmd/gitlab-prov-probe/main.go", true},
		// Control: the glob must NOT exempt a real prod binary under cmd/.
		// An over-broad "**/cmd/**" would silently drop the whole
		// controller from patch coverage; this asserts the "-probe" suffix
		// is load-bearing.
		{"gardenio/controller/cmd/gb/jira.go", false},
		{"gardenio/controller/cmd/slack-bridge/main.go", false},
	}
	for _, c := range cases {
		got := IsExcludedDefault(c.path)
		if got != c.want {
			t.Errorf("IsExcludedDefault(%q) = %v; want %v", c.path, got, c.want)
		}
	}
}
