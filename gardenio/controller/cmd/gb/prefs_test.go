package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestGetPreferencesMap_Empty(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{},
	}
	prefs, err := getPreferencesMap(bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prefs) != 0 {
		t.Fatalf("expected empty map, got %v", prefs)
	}
}

func TestGetPreferencesMap_Null(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{"preferences": "null"},
	}
	prefs, err := getPreferencesMap(bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prefs) != 0 {
		t.Fatalf("expected empty map, got %v", prefs)
	}
}

func TestGetPreferencesMap_Valid(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"preferences": `{"timezone":"America/Los_Angeles","default_project":"gardenio"}`,
		},
	}
	prefs, err := getPreferencesMap(bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prefs["timezone"] != "America/Los_Angeles" {
		t.Errorf("expected timezone 'America/Los_Angeles', got %q", prefs["timezone"])
	}
	if prefs["default_project"] != "gardenio" {
		t.Errorf("expected default_project 'gardenio', got %q", prefs["default_project"])
	}
}

func TestGetPreferencesMap_InvalidJSON(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{"preferences": "not-json"},
	}
	_, err := getPreferencesMap(bead)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
