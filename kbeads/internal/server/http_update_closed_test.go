package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestHandleUpdateBead_ClosedBead_DropsTransientFields verifies that PATCH
// /v1/beads/{id} on a closed bead silently drops transient operational fields
// (notes, agent_state, pod_phase, pod_name, pod_namespace, coop_url, backend)
// rather than overwriting them. Regression guard for ka-0000005253 — the
// status reporter keeps PATCHing during the K8s pod grace period after gb stop
// has closed the bead, and was flipping agent_state from "done" back to
// "working".
func TestHandleUpdateBead_ClosedBead_DropsTransientFields(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-closed"] = &model.Bead{
		ID:     "gd-closed",
		Title:  "Closed agent",
		Kind:   model.KindIssue,
		Type:   model.TypeTask,
		Status: model.StatusClosed,
		Notes:  "backend: coop\ncoop_url: http://orig:8080",
		Fields: json.RawMessage(`{"agent_state":"done","pod_phase":"Succeeded"}`),
	}

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-closed", map[string]any{
		"notes": "backend: coop\ncoop_url: http://stale:8080",
		"fields": map[string]any{
			"agent_state":   "working",
			"pod_phase":     "Running",
			"pod_name":      "pod-stale",
			"pod_namespace": "gardenio",
			"coop_url":      "http://stale:8080",
			"backend":       "coop",
		},
	})
	requireStatus(t, rec, 200)

	got := ms.beads["gd-closed"]
	if got.Notes != "backend: coop\ncoop_url: http://orig:8080" {
		t.Errorf("notes overwritten on closed bead: got %q", got.Notes)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["agent_state"] != "done" {
		t.Errorf("agent_state overwritten on closed bead: got %q, want done", fields["agent_state"])
	}
	if fields["pod_phase"] != "Succeeded" {
		t.Errorf("pod_phase overwritten on closed bead: got %q, want Succeeded", fields["pod_phase"])
	}
	for _, ev := range ms.events {
		if ev.Topic == "beads.bead.updated" {
			t.Errorf("unexpected beads.bead.updated event for no-op write to closed bead")
		}
	}
}

// TestHandleUpdateBead_Reopen_KeepsTransientFields verifies that a PATCH which
// REOPENS a closed bead (status set to a non-closed value) is NOT subject to the
// transient-field drop — the plan-gardener wake reopens a stopped gardener AND
// clears its terminal agent_state in one atomic PATCH, and the clear must land
// so the reopen re-emits AgentSpawn with a cleared state instead of looping the
// reconciler reopen→reap cycle. Regression guard for ka-0000001583.
func TestHandleUpdateBead_Reopen_KeepsTransientFields(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-wake"] = &model.Bead{
		ID:     "gd-wake",
		Title:  "Stopped plan gardener",
		Kind:   model.KindConfig, // an agent bead is kind=config, so the in_progress-assignee invariant does not apply
		Type:   "agent",
		Status: model.StatusClosed,
		// A realistic plan-gardener agent bead: agent/role/project are required by
		// the type:agent schema and are validated against the MERGED post-patch
		// fields on every update.
		Fields: json.RawMessage(`{"agent":"gardenio-plan-gardener-x","role":"plan-gardener","project":"gardenio","agent_state":"done","pod_phase":"succeeded"}`),
	}

	// The wake clears agent_state by DELETING it (JSON null / RFC-7386 merge),
	// not by setting "" — agent_state is an enum with no empty-string member, so
	// "" would 400 at the type-field validator. The reopen-skip must let this
	// delete through instead of stripping it.
	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-wake", map[string]any{
		"status": "in_progress",
		"fields": map[string]any{
			"agent_state":    nil, // the terminal-state clear (delete) that MUST survive
			"session_resume": "true",
			"prompt":         "please continue the plan",
		},
	})
	requireStatus(t, rec, 200)

	got := ms.beads["gd-wake"]
	if got.Status != model.StatusInProgress {
		t.Errorf("status = %q, want in_progress (reopen)", got.Status)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if av, ok := fields["agent_state"]; ok {
		t.Errorf("agent_state = %q still present on reopen, want DELETED (ka-0000001583)", av)
	}
	if fields["session_resume"] != "true" {
		t.Errorf("session_resume = %q, want true (reopen must apply wake fields)", fields["session_resume"])
	}
	if fields["prompt"] != "please continue the plan" {
		t.Errorf("prompt = %q, want the wake message", fields["prompt"])
	}
}

// TestHandleUpdateBead_ClosedBead_AllowsLegitimateEdits verifies that closed
// beads can still receive edits to non-transient fields (description, title,
// labels). Only the controller's status-reporter writes are filtered.
func TestHandleUpdateBead_ClosedBead_AllowsLegitimateEdits(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-closed-edit"] = &model.Bead{
		ID:          "gd-closed-edit",
		Title:       "Old title",
		Description: "Old description",
		Kind:        model.KindIssue,
		Type:        model.TypeTask,
		Status:      model.StatusClosed,
		Fields:      json.RawMessage(`{"agent_state":"done"}`),
	}

	newTitle := "Updated title"
	newDesc := "Updated description"
	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-closed-edit", map[string]any{
		"title":       newTitle,
		"description": newDesc,
		"fields": map[string]any{
			"agent_state": "working", // dropped
		},
	})
	requireStatus(t, rec, 200)

	got := ms.beads["gd-closed-edit"]
	if got.Title != newTitle {
		t.Errorf("title not updated: got %q, want %q", got.Title, newTitle)
	}
	if got.Description != newDesc {
		t.Errorf("description not updated: got %q, want %q", got.Description, newDesc)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["agent_state"] != "done" {
		t.Errorf("agent_state should be preserved on closed bead: got %q", fields["agent_state"])
	}
}
