package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gb lands gatehouse demo|apply (lands_gatehouse.go, gd-K62coPtKkX).

type gatehouseStub struct {
	created beadsapi.CreateBeadRequest
	reads   int
	answer  int // the read on which the reactor has answered; 0 never
}

func (s *gatehouseStub) CreateBeadDetail(_ context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	s.created = req
	return &beadsapi.BeadDetail{ID: "gd-req", Fields: map[string]string{"state": "pending"}}, nil
}

func (s *gatehouseStub) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	s.reads++
	if s.answer > 0 && s.reads >= s.answer {
		return &beadsapi.BeadDetail{ID: id, Fields: map[string]string{"state": "done", "outcome": "pass filed", "result_id": "gd-inv"}}, nil
	}
	return &beadsapi.BeadDetail{ID: id, Fields: map[string]string{"state": "pending"}}, nil
}

func TestFileGatehouseRequest_FilesInTheGatehouseAndWaits(t *testing.T) {
	s := &gatehouseStub{answer: 2}
	b, err := fileGatehouseRequest(context.Background(), s, "gatehouse", "Demo pass",
		map[string]any{"kind": "demo", "message_id": "gd-m1"}, time.Minute, func(time.Duration) {})
	if err != nil {
		t.Fatal(err)
	}
	if b.Fields["state"] != "done" || b.Fields["result_id"] != "gd-inv" || s.reads != 2 {
		t.Fatalf("answer = %v after %d reads", b.Fields, s.reads)
	}
	if s.created.Type != gatehouseRequestType || len(s.created.Labels) != 1 || s.created.Labels[0] != "project:gatehouse" || s.created.ShortTitle == "" {
		t.Fatalf("created = %+v", s.created)
	}
	var f map[string]any
	_ = json.Unmarshal(s.created.Fields, &f)
	if f["state"] != "pending" || f["message_id"] != "gd-m1" || f["kind"] != "demo" {
		t.Fatalf("fields = %v", f)
	}
	for _, k := range []string{"github_id", "github_login", "email"} {
		if _, ok := f[k]; ok {
			t.Fatalf("the steward named the person (%s): %v", k, f)
		}
	}
}

func TestFileGatehouseRequest_TimeoutLeavesItPending(t *testing.T) {
	s := &gatehouseStub{}
	b, err := fileGatehouseRequest(context.Background(), s, "gatehouse", "Demo pass",
		map[string]any{"kind": "demo", "message_id": "gd-m1"}, 6*time.Second, func(time.Duration) {})
	if err != nil || b.Fields["state"] != "pending" || s.reads != 3 {
		t.Fatalf("b = %v, err = %v, reads = %d", b, err, s.reads)
	}
}

func TestFileGatehouseRequest_NeedsAMessageAndALand(t *testing.T) {
	s := &gatehouseStub{}
	if _, err := fileGatehouseRequest(context.Background(), s, "gatehouse", "x", map[string]any{"kind": "demo", "message_id": " "}, 0, func(time.Duration) {}); err == nil {
		t.Fatal("no message: want an error")
	}
	if _, err := fileGatehouseRequest(context.Background(), s, "", "x", map[string]any{"kind": "demo", "message_id": "gd-m1"}, 0, func(time.Duration) {}); err == nil {
		t.Fatal("no land: want an error")
	}
}
