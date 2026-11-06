package main

import (
	"os"
	"strconv"
	"strings"
)

// Dollar-spend runaway surfacing (bug ka-0000001009 / C1). The gardener page
// already rolls up per-gardener dollar spend (gardener_spend.go) and lets an
// operator SORT by it, but a gardener quietly burning large $ while "making
// progress" is never FLAGGED — the operator has to click Spend-sort and eyeball
// the top. This file buckets the maintained gardener_spend_usd figure into a
// warn/over level so the chip can color itself and an over-budget gardener can
// bump NeedsAttention, mirroring the run-COUNT budget's gardenerBudgetBurn.Level.
//
// The thresholds are DOLLAR absolutes, operator-tunable via env so a fleet with
// a different cost profile can recalibrate without a code change. Defaults are
// calibrated against prod ground truth (docs/gardeners-camp/2026-08-17T1015:
// 5 open gardeners carry spend, ~$757 total, top gardener ~$582): warn well below
// the top gardener so a genuinely expensive loop lights up, over near it so the
// "runaway" state is reserved for the truly costly.

const (
	// gardenerSpendWarnEnv / gardenerSpendOverEnv override the default dollar
	// thresholds. A non-positive or unparseable value falls back to the default.
	gardenerSpendWarnEnv = "BEADS_VIEWER_GARDENER_SPEND_WARN_USD"
	gardenerSpendOverEnv = "BEADS_VIEWER_GARDENER_SPEND_OVER_USD"

	// gardenerSpendWarnDefaultUSD / gardenerSpendOverDefaultUSD are the built-in
	// dollar thresholds (see the ground-truth calibration above).
	gardenerSpendWarnDefaultUSD = 100.0
	gardenerSpendOverDefaultUSD = 250.0
)

// gardenerSpendThresholds returns the effective (warn, over) dollar thresholds,
// reading the env overrides and falling back to the defaults. `over` is floored
// at `warn` so a misconfigured over<warn can never invert the buckets (an "over"
// row would otherwise never be reachable, silently disabling the runaway flag).
func gardenerSpendThresholds() (warn, over float64) {
	warn = floatEnvOrDefault(gardenerSpendWarnEnv, gardenerSpendWarnDefaultUSD)
	over = floatEnvOrDefault(gardenerSpendOverEnv, gardenerSpendOverDefaultUSD)
	if over < warn {
		over = warn
	}
	return warn, over
}

// gardenerSpendLevel buckets a dollar figure into "" (below warn — no styling),
// "warn" (>= warn, < over) or "over" (>= over — the runaway state). Only called
// for a row/header that HasSpend, so an unsynced gardener never gets a level.
func gardenerSpendLevel(spendUSD float64) string {
	warn, over := gardenerSpendThresholds()
	switch {
	case spendUSD >= over:
		return "over"
	case spendUSD >= warn:
		return "warn"
	default:
		return ""
	}
}

// floatEnvOrDefault parses a positive float from env, returning def on
// empty/unparseable/non-positive input.
func floatEnvOrDefault(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return def
	}
	return f
}
