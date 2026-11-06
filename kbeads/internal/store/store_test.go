package store

import (
	"testing"
	"time"
)

// The seven option-struct Validate() methods below are pure, zero-dependency
// validators (no DB, no fixtures). Each fills defaults on the zero value and
// rejects its own distinct malformed input. The tests assert both: that a
// valid/zero value succeeds AND that the defaults are actually stamped, and
// that each error branch returns its specific sentinel.

func TestBackfillOperatedByLegacyOptions_Validate(t *testing.T) {
	t.Run("empty target rejected", func(t *testing.T) {
		o := &BackfillOperatedByLegacyOptions{Target: ""}
		if err := o.Validate(); err != errBackfillLegacyTargetRequired {
			t.Fatalf("Validate() err = %v, want errBackfillLegacyTargetRequired", err)
		}
	})

	t.Run("defaults filled", func(t *testing.T) {
		o := &BackfillOperatedByLegacyOptions{Target: "slack:U0OPERATOR1"}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if len(o.Statuses) != 1 || o.Statuses[0] != "open" {
			t.Errorf("Statuses = %v, want [open]", o.Statuses)
		}
		if o.BatchSize != 200 {
			t.Errorf("BatchSize = %d, want 200", o.BatchSize)
		}
		if o.MaxIterations != 5 {
			t.Errorf("MaxIterations = %d, want 5", o.MaxIterations)
		}
	})

	t.Run("negative batch and iterations reset to defaults", func(t *testing.T) {
		o := &BackfillOperatedByLegacyOptions{Target: "slack:U1", BatchSize: -3, MaxIterations: -1}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 200 {
			t.Errorf("BatchSize = %d, want 200", o.BatchSize)
		}
		if o.MaxIterations != 5 {
			t.Errorf("MaxIterations = %d, want 5", o.MaxIterations)
		}
	})

	t.Run("explicit values preserved", func(t *testing.T) {
		o := &BackfillOperatedByLegacyOptions{
			Target:        "slack:U1",
			Statuses:      []string{"open", "in_progress"},
			BatchSize:     50,
			MaxIterations: 9,
		}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if len(o.Statuses) != 2 || o.BatchSize != 50 || o.MaxIterations != 9 {
			t.Errorf("explicit values were overwritten: %+v", o)
		}
	})
}

func TestBackfillOperatedByLegacyOptions_TargetSubject(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"slack subject", "slack:U0OPERATOR1", "U0OPERATOR1"},
		{"oidc subject with at", "oidc:matt@acme.example", "matt@acme.example"},
		{"subject after first colon only", "kind:a:b", "a:b"},
		{"empty subject after colon", "slack:", ""},
		{"no colon returns empty", "U0OPERATOR1", ""},
		{"empty target returns empty", "", ""},
		{"leading colon", ":subject", "subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &BackfillOperatedByLegacyOptions{Target: tc.target}
			if got := o.TargetSubject(); got != tc.want {
				t.Errorf("TargetSubject(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

func TestBackfillParentChildDepsOptions_Validate(t *testing.T) {
	t.Run("negative since rejected", func(t *testing.T) {
		o := &BackfillParentChildDepsOptions{Since: -time.Hour}
		if err := o.Validate(); err != errBackfillParentChildSinceInvalid {
			t.Fatalf("Validate() err = %v, want errBackfillParentChildSinceInvalid", err)
		}
	})

	t.Run("zero since and batch filled to defaults", func(t *testing.T) {
		o := &BackfillParentChildDepsOptions{}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.Since != 180*24*time.Hour {
			t.Errorf("Since = %v, want 180d", o.Since)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})

	t.Run("negative batch reset to default, since preserved", func(t *testing.T) {
		o := &BackfillParentChildDepsOptions{Since: 48 * time.Hour, BatchSize: -1}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.Since != 48*time.Hour {
			t.Errorf("Since = %v, want 48h (preserved)", o.Since)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})

	t.Run("explicit values preserved", func(t *testing.T) {
		o := &BackfillParentChildDepsOptions{Since: time.Hour, BatchSize: 42}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.Since != time.Hour || o.BatchSize != 42 {
			t.Errorf("explicit values were overwritten: %+v", o)
		}
	})
}

func TestRepairReversedParentChildDepsOptions_Validate(t *testing.T) {
	t.Run("negative batch rejected", func(t *testing.T) {
		o := &RepairReversedParentChildDepsOptions{BatchSize: -1}
		if err := o.Validate(); err != errRepairReversedParentChildBatchInvalid {
			t.Fatalf("Validate() err = %v, want errRepairReversedParentChildBatchInvalid", err)
		}
	})

	t.Run("zero batch filled to default", func(t *testing.T) {
		o := &RepairReversedParentChildDepsOptions{}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})

	t.Run("explicit batch preserved", func(t *testing.T) {
		o := &RepairReversedParentChildDepsOptions{BatchSize: 100}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 100 {
			t.Errorf("BatchSize = %d, want 100", o.BatchSize)
		}
	})
}

func TestBackfillTriggeredByDepsOptions_Validate(t *testing.T) {
	t.Run("negative since rejected", func(t *testing.T) {
		o := &BackfillTriggeredByDepsOptions{Since: -time.Second}
		if err := o.Validate(); err != errBackfillTriggeredBySinceInvalid {
			t.Fatalf("Validate() err = %v, want errBackfillTriggeredBySinceInvalid", err)
		}
	})

	t.Run("zero since and batch filled to defaults", func(t *testing.T) {
		o := &BackfillTriggeredByDepsOptions{}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.Since != 24*time.Hour {
			t.Errorf("Since = %v, want 24h", o.Since)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})

	t.Run("negative batch reset to default, since preserved", func(t *testing.T) {
		o := &BackfillTriggeredByDepsOptions{Since: 12 * time.Hour, BatchSize: -5}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.Since != 12*time.Hour {
			t.Errorf("Since = %v, want 12h (preserved)", o.Since)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})
}

func TestCloseTriggerAuditBeadsOptions_Validate(t *testing.T) {
	t.Run("negative batch rejected", func(t *testing.T) {
		o := &CloseTriggerAuditBeadsOptions{BatchSize: -2}
		if err := o.Validate(); err != errCloseTriggerAuditBatchSize {
			t.Fatalf("Validate() err = %v, want errCloseTriggerAuditBatchSize", err)
		}
	})

	t.Run("zero batch filled to default", func(t *testing.T) {
		o := &CloseTriggerAuditBeadsOptions{}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})

	t.Run("explicit batch preserved", func(t *testing.T) {
		o := &CloseTriggerAuditBeadsOptions{BatchSize: 250}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 250 {
			t.Errorf("BatchSize = %d, want 250", o.BatchSize)
		}
	})
}

func TestBackfillOwnerOptions_Validate(t *testing.T) {
	t.Run("empty target rejected", func(t *testing.T) {
		o := &BackfillOwnerOptions{}
		if err := o.Validate(); err != errBackfillOwnerTargetRequired {
			t.Fatalf("Validate() err = %v, want errBackfillOwnerTargetRequired", err)
		}
	})

	t.Run("negative batch rejected", func(t *testing.T) {
		o := &BackfillOwnerOptions{Target: "slack:U1", BatchSize: -1}
		if err := o.Validate(); err != errBackfillOwnerBatchSizeInvalid {
			t.Fatalf("Validate() err = %v, want errBackfillOwnerBatchSizeInvalid", err)
		}
	})

	t.Run("defaults filled", func(t *testing.T) {
		o := &BackfillOwnerOptions{Target: "slack:U0OPERATOR1"}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 5000 {
			t.Errorf("BatchSize = %d, want 5000", o.BatchSize)
		}
		if o.UnownedSentinel != "system:unowned" {
			t.Errorf("UnownedSentinel = %q, want system:unowned", o.UnownedSentinel)
		}
	})

	t.Run("explicit values preserved", func(t *testing.T) {
		o := &BackfillOwnerOptions{
			Target:          "slack:U1",
			BatchSize:       10,
			UnownedSentinel: "system:custom",
		}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 10 || o.UnownedSentinel != "system:custom" {
			t.Errorf("explicit values were overwritten: %+v", o)
		}
	})
}

func TestBackfillOperatedByAgentsOptions_Validate(t *testing.T) {
	t.Run("negative batch rejected", func(t *testing.T) {
		o := &BackfillOperatedByAgentsOptions{BatchSize: -7}
		if err := o.Validate(); err != errBackfillOperatedByAgentsBatchSizeInvalid {
			t.Fatalf("Validate() err = %v, want errBackfillOperatedByAgentsBatchSizeInvalid", err)
		}
	})

	t.Run("zero batch filled to default", func(t *testing.T) {
		o := &BackfillOperatedByAgentsOptions{}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 500 {
			t.Errorf("BatchSize = %d, want 500", o.BatchSize)
		}
	})

	t.Run("explicit batch preserved", func(t *testing.T) {
		o := &BackfillOperatedByAgentsOptions{BatchSize: 33}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate() err = %v, want nil", err)
		}
		if o.BatchSize != 33 {
			t.Errorf("BatchSize = %d, want 33", o.BatchSize)
		}
	})
}

// TestUpsertOperatorEmailError_Error covers the structured error's message
// formatting — the one remaining statement-bearing method in store.go.
func TestUpsertOperatorEmailError_Error(t *testing.T) {
	err := &UpsertOperatorEmailError{Kind: "slack", Subject: "U0OPERATOR1"}
	want := "UpsertOperator: cannot derive email for kind=slack subject=U0OPERATOR1"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
