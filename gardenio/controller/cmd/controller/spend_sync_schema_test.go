package main

import (
	"testing"

	"gardenio/controller/internal/kbeadsschematest"
)

// TestSpendLimitFieldsAreKbeadsAccountFields: spend sync writes the fields of
// spendLimitFields onto the account bead in the same update as the spend, so a
// field kbeads' type:account does not declare fails the whole write (it did for
// every budgeted land key: "spend_limit_usd: unknown field").
func TestSpendLimitFieldsAreKbeadsAccountFields(t *testing.T) {
	budget := 500.0
	fields := spendLimitFields(liteLLMKeyInfo{MaxBudget: &budget, BudgetResetAt: "2026-12-01T00:00:00Z"})
	if len(fields) != 2 {
		t.Fatalf("control: spendLimitFields wrote %v, want both budget fields", fields)
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	kbeadsschematest.RequireFields(t, "account", names...)
}
