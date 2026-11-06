package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/wall"
)

// fakeWallReader serves the project list and the new agent bead, whose fields
// change after `after` reads (the controller deciding).
type fakeWallReader struct {
	projects map[string]beadsapi.ProjectInfo
	listErr  error
	before   map[string]string
	decided  map[string]string
	after    int
	reads    int
}

func (f *fakeWallReader) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return f.projects, f.listErr
}

func (f *fakeWallReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.reads++
	fields := f.before
	if f.reads > f.after {
		fields = f.decided
	}
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Fields: fields}, nil
}

func wallFarm() map[string]beadsapi.ProjectInfo {
	return map[string]beadsapi.ProjectInfo{
		"tomato-patch": {Name: "tomato-patch", LandKind: "tenant"},
		"gardenio":     {Name: "gardenio"},
	}
}

func TestAwaitSpawnWall_PrintsTheRefusal(t *testing.T) {
	f := &fakeWallReader{projects: wallFarm(), after: 2,
		before:  map[string]string{"agent_state": "spawning"},
		decided: map[string]string{"agent_state": wall.AgentState, wall.FieldCode: wall.CodePurseWall, wall.FieldReason: "the purse is empty"},
	}
	r, refused := awaitSpawnWall(context.Background(), f, "tomato-patch", "kd-ag1", time.Second, time.Millisecond)
	if !refused || r.Code != wall.CodePurseWall || r.Reason != "the purse is empty" {
		t.Fatalf("got %+v refused=%v", r, refused)
	}
	err := reportSpawnWallRefusal("crew-1", "kd-ag1", "tomato-patch", "crew", "", r)
	if err == nil {
		t.Error("a refused spawn must exit non-zero")
	}
}

func TestAwaitSpawnWall_AdmittedReturns(t *testing.T) {
	f := &fakeWallReader{projects: wallFarm(), after: 1,
		before:  map[string]string{"agent_state": "spawning"},
		decided: map[string]string{"agent_state": "spawning", "pod_name": "crew-tomato-patch-crew-1"},
	}
	if _, refused := awaitSpawnWall(context.Background(), f, "tomato-patch", "kd-ag1", time.Second, time.Millisecond); refused {
		t.Fatal("an admitted spawn is not refused")
	}
	if f.reads != 2 {
		t.Errorf("reads = %d, want 2 (stop once the pod exists)", f.reads)
	}
}

func TestAwaitSpawnWall_TimesOut(t *testing.T) {
	f := &fakeWallReader{projects: wallFarm(), after: 1 << 30, before: map[string]string{"agent_state": "spawning"}}
	start := time.Now()
	if _, refused := awaitSpawnWall(context.Background(), f, "tomato-patch", "kd-ag1", 20*time.Millisecond, 5*time.Millisecond); refused {
		t.Fatal("no verdict is not a refusal")
	}
	if time.Since(start) > time.Second {
		t.Error("the wait must end at its deadline")
	}
}

func TestAwaitSpawnWall_OperatorLandsDoNotWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
		listErr error
	}{{"operator land", "gardenio", nil}, {"unknown project", "nowhere", nil}, {"unreadable", "tomato-patch", errors.New("down")}} {
		f := &fakeWallReader{projects: wallFarm(), listErr: tc.listErr}
		if _, refused := awaitSpawnWall(context.Background(), f, tc.project, "kd-ag1", time.Second, time.Millisecond); refused || f.reads != 0 {
			t.Errorf("%s: refused=%v reads=%d, want no wait", tc.name, refused, f.reads)
		}
	}
}
