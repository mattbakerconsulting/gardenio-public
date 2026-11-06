package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"k8s.io/apimachinery/pkg/api/resource"
)

// roleValidityReport is the role-side mirror of projectValidityReport
// (project_validity.go): a read-path HEALTH signal for a type:role config
// bead, NOT a parallel schema. The authoritative schema lives in the daemon
// type:role TypeConfig. This report catches the same class of problems the
// daemon CANNOT see and the controller silently TOLERATES when it parses a
// role bead (beadsapi/roles.go, ListRoleBeads):
//
//	(a) JSON fields that decode-fail into their Go struct and are SILENTLY
//	    DROPPED — secrets ([]SecretEntry), env ([]EnvEntry), env_json
//	    (map[string]string), claude_config (map[string]json.RawMessage). The
//	    roles parser does `if json.Unmarshal(...) == nil { ... }`, so a shape
//	    mismatch reads as empty with no error. This report re-parses the raw
//	    field and flags it.
//	(b) cpu/memory strings stored as plain "string" by the daemon that must be
//	    valid Kubernetes resource quantities, or the pod spec is broken.
//	(c) account references that don't resolve to a live type:account bead — a
//	    dangling reference that breaks API routing at spawn time.
//
// It drives the config-validity badge on the /roles index cards (design doc
// §8.5). Computed from the RAW bead fields (map[string]string), NOT a parsed
// RoleInfo, because RoleInfo has already discarded every malformed field —
// which is exactly what we want to surface. The shared projectValidityCheck /
// validitySeverity types and the OK/Badge/ErrorCount/WarningCount helpers from
// project_validity.go are reused verbatim so the role and project badges render
// identically.
type roleValidityReport struct {
	// Checks is the list of findings, deterministically ordered (by field then
	// message). Empty when the config is fully valid.
	Checks []projectValidityCheck
}

// OK reports whether the config has no findings at all (green badge).
func (r roleValidityReport) OK() bool { return len(r.Checks) == 0 }

// ErrorCount returns the number of error-severity findings.
func (r roleValidityReport) ErrorCount() int {
	n := 0
	for _, c := range r.Checks {
		if c.Severity == severityError {
			n++
		}
	}
	return n
}

// WarningCount returns the number of warning-severity findings.
func (r roleValidityReport) WarningCount() int {
	return len(r.Checks) - r.ErrorCount()
}

// Badge returns the badge color the index should render: "green" (no
// findings), "yellow" (warnings only), "red" (one or more errors). Matches
// projectValidityReport.Badge exactly.
func (r roleValidityReport) Badge() string {
	switch {
	case r.ErrorCount() > 0:
		return "red"
	case len(r.Checks) > 0:
		return "yellow"
	default:
		return "green"
	}
}

// buildRoleValidityReport computes the health report for one role's raw
// config-bead fields. knownAccounts is the set of live type:account bead names
// used to resolve the "account" reference and the "account_whitelist" entries
// (pass nil to skip the reference checks — e.g. when the account list could not
// be fetched). The result is deterministically ordered. Mirrors
// buildProjectValidityReport but over the role bead's field set.
func buildRoleValidityReport(fields map[string]string, knownAccounts map[string]struct{}) roleValidityReport {
	var report roleValidityReport
	add := func(field string, sev validitySeverity, format string, args ...any) {
		report.Checks = append(report.Checks, projectValidityCheck{
			Field:    field,
			Severity: sev,
			Message:  fmt.Sprintf(format, args...),
		})
	}

	// (a) JSON shape checks. Each maps a role bead field to the Go type the
	// roles parser (beadsapi/roles.go, ListRoleBeads) expects. A non-empty
	// value that doesn't decode into that type is silently dropped.
	shapeFields := []jsonShapeField{
		{"secrets", shapeUnmarshal[[]beadsapi.SecretEntry]},
		{"env", shapeUnmarshal[[]beadsapi.EnvEntry]},
		{"env_json", shapeUnmarshal[map[string]string]},
		{"claude_config", shapeUnmarshal[map[string]json.RawMessage]},
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
	// at spawn time. The account_whitelist entries are checked the same way:
	// each named account must resolve, else the spawn-time intersection silently
	// drops it (or fails closed if the whole list is dangling). Both skipped
	// entirely when knownAccounts is nil (couldn't load).
	if knownAccounts != nil {
		if acct := fields["account"]; acct != "" {
			if _, ok := knownAccounts[acct]; !ok {
				add("account", severityError, "references account %q, which does not resolve to a live type:account bead", acct)
			}
		}
		if raw := fields["account_whitelist"]; raw != "" {
			for _, acct := range parseRoleAccountList(raw) {
				if _, ok := knownAccounts[acct]; !ok {
					add("account_whitelist", severityError, "lists account %q, which does not resolve to a live type:account bead", acct)
				}
			}
		}
	}

	sort.SliceStable(report.Checks, func(i, j int) bool {
		if report.Checks[i].Field != report.Checks[j].Field {
			return report.Checks[i].Field < report.Checks[j].Field
		}
		return report.Checks[i].Message < report.Checks[j].Message
	})
	return report
}

// parseRoleAccountList parses the account_whitelist field, which the roles
// parser (beadsapi/roles.go parseStringList) accepts as either a JSON array of
// strings or a comma-separated string. We re-implement the lax parse here
// rather than reaching into beadsapi so the validator stays a pure read-path
// helper over raw fields. A JSON-array shape mismatch is surfaced by the
// shapeFields check above, so here we only need the value list for the
// reference check; malformed JSON degrades to a comma-split of the raw string.
func parseRoleAccountList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var list []string
		if json.Unmarshal([]byte(raw), &list) == nil {
			return trimAndCompact(list)
		}
		// Fall through to comma-split on malformed JSON.
	}
	return trimAndCompact(strings.Split(raw, ","))
}

// trimAndCompact trims each element and drops the empties, preserving order.
func trimAndCompact(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
