package main

import (
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purge"
)

func purgeEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestLandsPurge_OffByDefault(t *testing.T) {
	getenv := purgeEnv(nil)
	s, err := newLandsPurge(&beadsapi.Client{}, nil, "", getenv, quietLogger())
	if s != nil || err != nil {
		t.Fatalf("off: %v, %v", s, err)
	}
	if slices.Contains(controllerMachineRolesFor(getenv), landsProvisionerMachineRole) {
		t.Fatal("the provisioner's role is expected while the sweep and provisioner are off")
	}
}

func TestLandsPurge_DefaultsAndOverrides(t *testing.T) {
	getenv := purgeEnv(map[string]string{"LANDS_PURGE_ENABLED": "true"})
	s, err := newLandsPurge(&beadsapi.Client{}, nil, "coop/sessions", getenv, quietLogger())
	if err != nil || s == nil {
		t.Fatalf("on: %v, %v", s, err)
	}
	if s.Retention != purge.Days(30) || s.RecordsRetention != purge.Days(730) || s.ApplicationRetention != purge.Days(90) {
		t.Fatalf("defaults = %s, %s, %s", s.Retention, s.RecordsRetention, s.ApplicationRetention)
	}
	if !s.DryRun {
		t.Fatal("the sweep must be a dry run unless LANDS_PURGE_DRY_RUN=false")
	}
	if !slices.Contains(controllerMachineRolesFor(getenv), landsProvisionerMachineRole) {
		t.Fatal("the sweep needs the provisioner's machine role")
	}

	getenv = purgeEnv(map[string]string{
		"LANDS_PURGE_ENABLED": "true", "LANDS_RETENTION_DAYS": "14",
		"LANDS_RECORDS_RETENTION_DAYS": "365", "LANDS_APPLICATION_RETENTION_DAYS": "60",
	})
	s, err = newLandsPurge(&beadsapi.Client{}, nil, "", getenv, quietLogger())
	if err != nil || s.Retention != purge.Days(14) || s.RecordsRetention != purge.Days(365) || s.ApplicationRetention != purge.Days(60) {
		t.Fatalf("overrides: %+v, %v", s, err)
	}
}

func TestLandsPurge_DryRun(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "yes": true, "flase": true, "false": false, " FALSE ": false} {
		getenv := purgeEnv(map[string]string{"LANDS_PURGE_ENABLED": "true", "LANDS_PURGE_DRY_RUN": v})
		s, err := newLandsPurge(&beadsapi.Client{}, nil, "", getenv, quietLogger())
		if err != nil || s == nil || s.DryRun != want {
			t.Fatalf("LANDS_PURGE_DRY_RUN=%q: %+v, %v; want dry run %v", v, s, err, want)
		}
	}
	// A dry run alone does not turn the sweep on.
	if s, err := newLandsPurge(&beadsapi.Client{}, nil, "", purgeEnv(map[string]string{"LANDS_PURGE_DRY_RUN": "true"}), quietLogger()); s != nil || err != nil {
		t.Fatalf("dry run without enabled: %+v, %v", s, err)
	}
}

func TestLandsPurge_RefusesBadPeriods(t *testing.T) {
	for _, env := range []map[string]string{
		{"LANDS_RETENTION_DAYS": "0"},
		{"LANDS_RETENTION_DAYS": "thirty"},
		{"LANDS_APPLICATION_RETENTION_DAYS": "-1"},
		{"LANDS_RETENTION_DAYS": "60", "LANDS_RECORDS_RETENTION_DAYS": "30"},
	} {
		env["LANDS_PURGE_ENABLED"] = "true"
		if s, err := newLandsPurge(&beadsapi.Client{}, nil, "", purgeEnv(env), quietLogger()); err == nil {
			t.Fatalf("%v: want an error, got %+v", env, s)
		}
	}
}
