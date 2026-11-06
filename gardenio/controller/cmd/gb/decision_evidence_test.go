package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEvidenceBeadIDs(t *testing.T) {
	parse := func(args ...string) ([]string, error) {
		c := &cobra.Command{}
		c.Flags().StringArray("evidence", nil, "")
		for _, a := range args {
			if err := c.Flags().Set("evidence", a); err != nil {
				t.Fatal(err)
			}
		}
		return evidenceBeadIDs(c)
	}
	if got, err := parse(); err != nil || got != nil {
		t.Errorf("no flag: %v %v", got, err)
	}
	got, err := parse("gd-RnAtOyYOk0", "gd-DCtOsPoF2V gd-RnAtOyYOk0")
	if err != nil || strings.Join(got, ",") != "gd-RnAtOyYOk0,gd-DCtOsPoF2V" {
		t.Errorf("repeatable, comma/space lists, de-duplicated: %v %v", got, err)
	}
	// The requesting task is always read and takes the third slot, so a
	// third --evidence bead would be dropped: refuse it instead.
	if _, err := parse("gd-RnAtOyYOk0", "gd-DCtOsPoF2V,ka-0000001617"); err == nil || !strings.Contains(err.Error(), "at most 2") {
		t.Errorf("three --evidence beads must be refused, got %v", err)
	}
	if _, err := parse("https://gardenio.clonk.tv/image/x.png"); err == nil {
		t.Error("a URL is not a bead id")
	}
}
