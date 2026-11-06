package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"k8s.io/apimachinery/pkg/api/resource"
)

// projectValidityReport is a read-path HEALTH signal for a project's config
// bead — NOT a parallel schema. The authoritative schema lives in the daemon
// type:project TypeConfig (kbeads/internal/server/config.go), which rejects
// unknown keys / wrong primitive types on write. This report catches the class
// of problems the daemon CANNOT see and the controller silently TOLERATES:
//
//	(a) JSON fields that are valid JSON to the daemon ("type":"json") but do
//	    not parse into their expected Go struct — SecretEntry, RepoEntry,
//	    PrewarmedPoolConfig, ResourceMappingEntry, etc. The controller parser
//	    (beadsapi/projects.go) does `if json.Unmarshal(...) == nil { ... }`,
//	    so a shape mismatch is SILENTLY DROPPED — the field reads as empty and
//	    no error surfaces. This report re-parses the raw field and flags it.
//	(b) cpu/memory strings that are stored as plain "string" by the daemon but
//	    must be valid Kubernetes resource quantities, or the pod spec is wrong.
//	(c) account references that don't resolve to a live type:account bead — a
//	    dangling reference that breaks API routing at spawn time.
//	(d) a budget_usd the daemon accepts as a number but that is not a usable
//	    budget (0, negative, NaN/Inf), and a budget_period outside month|7d.
//
// It drives the config-validity badge on the hub header + index cards (design
// doc §5.3). The report is computed from the RAW bead fields (map[string]string
// from BeadDetail.Fields), NOT from a parsed ProjectInfo — because ProjectInfo
// has already discarded every malformed field, which is exactly what we want to
// surface.
type projectValidityReport struct {
	// Checks is the list of findings, deterministically ordered (by field then
	// message). Empty when the config is fully valid.
	Checks []projectValidityCheck
}

// validitySeverity classifies a finding. error = the controller silently loses
// this config (malformed JSON, bad quantity, dangling reference). warning =
// suspicious but tolerated.
type validitySeverity string

const (
	severityError   validitySeverity = "error"
	severityWarning validitySeverity = "warning"
)

// projectValidityCheck is a single finding in a projectValidityReport.
type projectValidityCheck struct {
	Field    string           // bead field name, e.g. "secrets", "cpu_limit", "account"
	Severity validitySeverity // error | warning
	Message  string           // human-readable explanation
}

// OK reports whether the config has no findings at all (green badge).
func (r projectValidityReport) OK() bool { return len(r.Checks) == 0 }

// ErrorCount returns the number of error-severity findings. Drives the red
// badge: ErrorCount > 0 → red, else any warnings → yellow, else green.
func (r projectValidityReport) ErrorCount() int {
	n := 0
	for _, c := range r.Checks {
		if c.Severity == severityError {
			n++
		}
	}
	return n
}

// WarningCount returns the number of warning-severity findings.
func (r projectValidityReport) WarningCount() int {
	return len(r.Checks) - r.ErrorCount()
}

// Badge returns the badge color the hub/index should render: "green" (no
// findings), "yellow" (warnings only), "red" (one or more errors).
func (r projectValidityReport) Badge() string {
	switch {
	case r.ErrorCount() > 0:
		return "red"
	case len(r.Checks) > 0:
		return "yellow"
	default:
		return "green"
	}
}

// jsonShapeField names a JSON bead field and the Go type the controller parses
// it into. The validator round-trips the raw value through json.Unmarshal into
// that type to detect a silent shape mismatch.
type jsonShapeField struct {
	name  string
	parse func(raw string) error
}

// shapeUnmarshal parses raw into a value of type T, mirroring EXACTLY the
// controller parser's lax json.Unmarshal (beadsapi/projects.go). It deliberately
// does NOT use DisallowUnknownFields: the controller tolerates unknown keys, so
// flagging them here would yellow/red a config that actually works. We surface
// only what the controller genuinely drops — a fundamental shape mismatch
// (object vs array, string-into-int, truncated JSON) that makes json.Unmarshal
// itself return an error, which is exactly the `json.Unmarshal(...) == nil`
// guard the controller uses to silently skip the field. Trailing garbage after
// the first value is rejected because json.Unmarshal also rejects it.
func shapeUnmarshal[T any](raw string) error {
	var v T
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing data after JSON value")
	}
	return nil
}

// buildProjectValidityReport computes the health report for one project's raw
// config-bead fields. knownAccounts is the set of live type:account bead names
// used to resolve the "account" reference (pass nil to skip the reference check
// — e.g. when the account list could not be fetched). The result is
// deterministically ordered.
func buildProjectValidityReport(fields map[string]string, knownAccounts map[string]struct{}) projectValidityReport {
	var report projectValidityReport
	add := func(field string, sev validitySeverity, format string, args ...any) {
		report.Checks = append(report.Checks, projectValidityCheck{
			Field:    field,
			Severity: sev,
			Message:  fmt.Sprintf(format, args...),
		})
	}

	// (a) JSON shape checks. Each maps a bead field to the Go type the
	// controller parser (beadsapi/projects.go) expects. A non-empty value that
	// doesn't decode into that type is silently dropped by the controller.
	shapeFields := []jsonShapeField{
		{"secrets", shapeUnmarshal[[]beadsapi.SecretEntry]},
		{"env", shapeUnmarshal[[]beadsapi.EnvEntry]},
		{"repos", shapeUnmarshal[[]beadsapi.RepoEntry]},
		{"resource_mapping", shapeUnmarshal[[]beadsapi.ResourceMappingEntry]},
		{"prewarmed_pool", shapeUnmarshal[beadsapi.PrewarmedPoolConfig]},
		{"channel_roles", shapeUnmarshal[map[string]string]},
		{"channel_modes", shapeUnmarshal[map[string]string]},
		{"channel_runtime_class", shapeUnmarshal[map[string]string]},
		{"claude_config", shapeUnmarshal[map[string]json.RawMessage]},
		{"env_json", shapeUnmarshal[map[string]string]},
	}
	for _, f := range shapeFields {
		raw := fields[f.name]
		if raw == "" {
			continue // absent/empty is valid — field is optional
		}
		if err := f.parse(raw); err != nil {
			add(f.name, severityError, "malformed JSON (silently dropped by the controller): %v", err)
		}
	}

	// (b) Kubernetes resource-quantity checks. The daemon stores these as plain
	// strings; an invalid quantity produces a broken pod spec. Empty = inherit
	// the default, which is valid.
	for _, field := range []string{"cpu_request", "cpu_limit", "memory_request", "memory_limit"} {
		raw := fields[field]
		if raw == "" {
			continue
		}
		if _, err := resource.ParseQuantity(raw); err != nil {
			add(field, severityError, "%q is not a valid Kubernetes quantity: %v", raw, err)
		}
	}

	// (c) Account reference resolution. A non-empty account that doesn't match
	// a live type:account bead is a dangling reference — API routing will fail
	// at spawn time. Skipped entirely when knownAccounts is nil (couldn't load).
	if knownAccounts != nil {
		if acct := fields["account"]; acct != "" {
			if _, ok := knownAccounts[acct]; !ok {
				add("account", severityError, "references account %q, which does not resolve to a live type:account bead", acct)
			}
		}
	}

	// (d) Project budget (decision gd-PtNuTyjQoN). Blank = no budget. A set
	// budget must be a finite number > 0: the daemon accepts any number, and a
	// stored 0 would read as a fake "$0 remaining" instead of "no budget".
	if raw := fields["budget_usd"]; raw != "" {
		if _, err := parseBudgetUSD(raw); err != nil {
			add("budget_usd", severityError, "%v", err)
		}
	}
	if p := fields["budget_period"]; p != "" && p != "month" && p != "7d" {
		add("budget_period", severityError, "%q is not a valid budget period (must be month or 7d)", p)
	}

	sort.SliceStable(report.Checks, func(i, j int) bool {
		if report.Checks[i].Field != report.Checks[j].Field {
			return report.Checks[i].Field < report.Checks[j].Field
		}
		return report.Checks[i].Message < report.Checks[j].Message
	})
	return report
}

// parseBudgetUSD parses a project budget_usd value (US dollars). It accepts a
// plain decimal number and rejects anything that is not a finite amount above
// zero; the error reads as a field message. Blank means "no budget" and is the
// caller's case, not this function's.
func parseBudgetUSD(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%q is not a dollar amount (e.g. 250 or 99.50)", raw)
	}
	if v <= 0 {
		return 0, fmt.Errorf("budget must be greater than $0; leave it blank for no budget")
	}
	return v, nil
}
