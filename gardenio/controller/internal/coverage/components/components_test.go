package components

import "testing"

func TestOf(t *testing.T) {
	cases := []struct {
		path string
		want Component
	}{
		{"gardenio/controller/internal/foo/bar.go", Controller},
		{"gardenio/controller/cmd/gb/main.go", Controller},
		{"kbeads/cmd/kd/main.go", Kbeads},
		{"kbeads/internal/store/store.go", Kbeads},
		{"coop/crates/cli/src/main.rs", Coop},
		{"scripts/coverage/merge.sh", None},
		{"docs/research/foo.md", None},
		{".github/workflows/ci.yml", None},
		{".gitlab-ci.yml", None},
		{"Makefile", None},
		{"README.md", None},
		{"gardenio/helm/gardenio/Chart.yaml", None},
	}
	for _, c := range cases {
		got := Of(c.path)
		if got != c.want {
			t.Errorf("Of(%q) = %q; want %q", c.path, got, c.want)
		}
	}
}

func TestIsGated(t *testing.T) {
	if !IsGated(Controller) || !IsGated(Kbeads) || !IsGated(Coop) {
		t.Error("Controller/Kbeads/Coop must be gated")
	}
	if IsGated(None) {
		t.Error("None must NOT be gated")
	}
}
