package forgebeads

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type edge struct{ from, to, typ, by string }

type fakeDeps struct {
	edges []edge
	fail  map[string]error // by dep type
}

func (f *fakeDeps) AddDependency(_ context.Context, from, to, typ, by string) error {
	f.edges = append(f.edges, edge{from, to, typ, by})
	return f.fail[typ]
}

func TestLinkTaskToMR_WritesBothEdges(t *testing.T) {
	d := &fakeDeps{}
	if err := LinkTaskToMR(context.Background(), d, "gd-task", "gd-mr", "system:test"); err != nil {
		t.Fatal(err)
	}
	want := []edge{
		{"gd-task", "gd-mr", "produces", "system:test"},
		{"gd-mr", "gd-task", "triggered-by", "system:test"},
	}
	if len(d.edges) != 2 || d.edges[0] != want[0] || d.edges[1] != want[1] {
		t.Fatalf("edges = %+v, want %+v", d.edges, want)
	}
}

func TestLinkTaskToMR_ForwardFailureStillWritesReverse(t *testing.T) {
	d := &fakeDeps{fail: map[string]error{"produces": errors.New("duplicate")}}
	err := LinkTaskToMR(context.Background(), d, "gd-task", "gd-mr", "system:test")
	if err == nil || !strings.Contains(err.Error(), "produces dep: duplicate") {
		t.Fatalf("err = %v, want produces failure", err)
	}
	if len(d.edges) != 2 {
		t.Fatalf("reverse edge skipped after forward failure: %+v", d.edges)
	}
}

type fakeGetter struct {
	bead *beadsapi.BeadDetail
	err  error
}

func (f fakeGetter) GetBead(context.Context, string) (*beadsapi.BeadDetail, error) {
	return f.bead, f.err
}

func TestGetBeadIfExists(t *testing.T) {
	ctx := context.Background()
	b := &beadsapi.BeadDetail{ID: "gd-x"}
	if got, err := GetBeadIfExists(ctx, fakeGetter{bead: b}, "gd-x"); err != nil || got != b {
		t.Fatalf("found: got %v, %v", got, err)
	}
	notFound := &beadsapi.APIError{StatusCode: http.StatusNotFound}
	if got, err := GetBeadIfExists(ctx, fakeGetter{err: notFound}, "gd-x"); err != nil || got != nil {
		t.Fatalf("404 must be (nil, nil), got %v, %v", got, err)
	}
	boom := &beadsapi.APIError{StatusCode: http.StatusInternalServerError}
	if _, err := GetBeadIfExists(ctx, fakeGetter{err: boom}, "gd-x"); err == nil {
		t.Fatal("500 must surface as an error")
	}
}
