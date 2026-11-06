package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// AccountTriggerSource implements TriggerSource for account cost/usage triggers.
// It polls account beads and checks their fields against thresholds defined in
// trigger beads. Supports spend thresholds, utilization warnings, rate-limit
// detection, and account recovery events.
type AccountTriggerSource struct {
	logger *slog.Logger

	mu     sync.Mutex
	daemon TriggerBeadClient // cached from the most recent LoadTriggers call
}

// accountTriggerData holds account-specific fields parsed from a trigger bead.
type accountTriggerData struct {
	// ConditionType defines what to check:
	//   "spend_threshold"      — total_cost_usd exceeds threshold
	//   "utilization_threshold" — utilization_5h or utilization_7d exceeds threshold
	//   "all_rate_limited"     — all accounts have throttle_status == "rejected"
	//   "account_recovered"    — account throttle_status changed from "rejected" to "allowed"
	ConditionType string
	// Field is the account bead field to check (e.g. "total_cost_usd", "utilization_5h").
	// Used by spend_threshold and utilization_threshold.
	Field string
	// Threshold is the numeric value to compare against.
	Threshold float64
	// Op is the comparison operator: ">", ">=", "<", "<=", "==". Defaults to ">=".
	Op string
	// AccountFilter limits the check to a specific account name (optional).
	// Empty means check all accounts.
	AccountFilter string
}

// NewAccountTriggerSource creates a new account trigger source.
func NewAccountTriggerSource(logger *slog.Logger) *AccountTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountTriggerSource{
		logger: logger,
	}
}

func (s *AccountTriggerSource) Name() string { return "account" }

// LoadTriggers fetches all enabled trigger beads with label "trigger:account".
func (s *AccountTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	s.mu.Lock()
	s.daemon = daemon
	s.mu.Unlock()

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{"trigger:account"},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}

	var triggers []TriggerDef
	for _, bead := range result.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}

		condType := bead.Fields["condition_type"]
		if condType == "" {
			s.logger.Warn("account trigger: missing condition_type, skipping",
				"trigger", bead.ID)
			continue
		}

		t := ParseTriggerDef(bead, "account")
		data := accountTriggerData{
			ConditionType: condType,
			Field:         bead.Fields["field"],
			Op:            bead.Fields["op"],
			AccountFilter: bead.Fields["account_filter"],
		}
		if data.Op == "" {
			data.Op = ">="
		}

		if raw := bead.Fields["threshold"]; raw != "" {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				data.Threshold = v
			} else {
				s.logger.Warn("account trigger: invalid threshold",
					"trigger", bead.ID, "threshold", raw, "error", err)
				continue
			}
		}

		t.SourceData = data
		triggers = append(triggers, t)
	}
	return triggers, nil
}

// Evaluate checks account beads against the trigger's conditions.
func (s *AccountTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(accountTriggerData)
	if !ok {
		return nil, nil
	}

	daemon := s.getDaemon()
	if daemon == nil {
		return nil, nil
	}

	// Fetch all account beads.
	accounts, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    100,
	})
	if err != nil {
		return nil, fmt.Errorf("listing account beads: %w", err)
	}

	switch data.ConditionType {
	case "spend_threshold":
		return s.evalSpendThreshold(data, accounts.Beads)
	case "utilization_threshold":
		return s.evalUtilizationThreshold(data, accounts.Beads)
	case "all_rate_limited":
		return s.evalAllRateLimited(data, accounts.Beads)
	case "account_recovered":
		return s.evalAccountRecovered(data, accounts.Beads)
	default:
		s.logger.Warn("account trigger: unknown condition_type",
			"trigger", trig.ID, "type", data.ConditionType)
		return nil, nil
	}
}

// evalSpendThreshold checks if any account's spend field exceeds the threshold.
func (s *AccountTriggerSource) evalSpendThreshold(data accountTriggerData, accounts []*beadsapi.BeadDetail) ([]TriggerMatch, error) {
	field := data.Field
	if field == "" {
		field = "total_cost_usd"
	}

	var matches []TriggerMatch
	for _, acct := range accounts {
		if data.AccountFilter != "" && acct.Title != data.AccountFilter {
			continue
		}

		raw := acct.Fields[field]
		if raw == "" {
			continue
		}
		val, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}

		if !compareFloat(val, data.Op, data.Threshold) {
			continue
		}

		key := fmt.Sprintf("spend:%s:%s:%.0f", acct.Title, field, data.Threshold)
		matches = append(matches, accountMatchFromBead(acct, key, "spend_threshold", field, val))
	}
	return matches, nil
}

// evalUtilizationThreshold checks if any account's utilization exceeds the threshold.
func (s *AccountTriggerSource) evalUtilizationThreshold(data accountTriggerData, accounts []*beadsapi.BeadDetail) ([]TriggerMatch, error) {
	field := data.Field
	if field == "" {
		field = "utilization_5h"
	}

	var matches []TriggerMatch
	for _, acct := range accounts {
		if data.AccountFilter != "" && acct.Title != data.AccountFilter {
			continue
		}

		raw := acct.Fields[field]
		if raw == "" {
			continue
		}
		val, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}

		if !compareFloat(val, data.Op, data.Threshold) {
			continue
		}

		key := fmt.Sprintf("util:%s:%s:%.2f", acct.Title, field, data.Threshold)
		matches = append(matches, accountMatchFromBead(acct, key, "utilization_threshold", field, val))
	}
	return matches, nil
}

// evalAllRateLimited checks if ALL accounts have throttle_status == "rejected".
func (s *AccountTriggerSource) evalAllRateLimited(data accountTriggerData, accounts []*beadsapi.BeadDetail) ([]TriggerMatch, error) {
	if len(accounts) == 0 {
		return nil, nil
	}

	allLimited := true
	var limitedNames []string
	for _, acct := range accounts {
		if data.AccountFilter != "" && acct.Title != data.AccountFilter {
			continue
		}
		status := acct.Fields["throttle_status"]
		if status == "rejected" {
			limitedNames = append(limitedNames, acct.Title)
		} else {
			allLimited = false
		}
	}

	if !allLimited || len(limitedNames) == 0 {
		return nil, nil
	}

	key := fmt.Sprintf("all_rate_limited:%d", len(limitedNames))
	names := strings.Join(limitedNames, ",")

	vars := map[string]string{
		"{{condition}}":     "all_rate_limited",
		"{{account_count}}": strconv.Itoa(len(limitedNames)),
		"{{account_names}}": names,
	}
	fields := map[string]string{
		"account_event_key": key,
		"condition":         "all_rate_limited",
		"account_names":     names,
	}

	return []TriggerMatch{{
		Key:          key,
		Title:        fmt.Sprintf("All %d accounts rate-limited", len(limitedNames)),
		Fields:       fields,
		Labels:       []string{"source:account"},
		TemplateVars: vars,
	}}, nil
}

// evalAccountRecovered checks if any account's throttle_status is "allowed"
// (indicating recovery from rate-limiting).
func (s *AccountTriggerSource) evalAccountRecovered(data accountTriggerData, accounts []*beadsapi.BeadDetail) ([]TriggerMatch, error) {
	var matches []TriggerMatch
	for _, acct := range accounts {
		if data.AccountFilter != "" && acct.Title != data.AccountFilter {
			continue
		}

		status := acct.Fields["throttle_status"]
		if status != "allowed" {
			continue
		}

		// Only trigger if the account was previously rate-limited
		// (has a rotation_count > 0 or exhausted_at set).
		rotCount := acct.Fields["rotation_count"]
		exhaustedAt := acct.Fields["exhausted_at"]
		if rotCount == "" && exhaustedAt == "" {
			continue // never been rate-limited, not a recovery
		}

		key := fmt.Sprintf("recovered:%s", acct.Title)
		matches = append(matches, accountMatchFromBead(acct, key, "account_recovered", "throttle_status", 0))
	}
	return matches, nil
}

// accountMatchFromBead creates a TriggerMatch from an account bead.
func accountMatchFromBead(acct *beadsapi.BeadDetail, key, condition, field string, value float64) TriggerMatch {
	vars := map[string]string{
		"{{account_name}}":     acct.Title,
		"{{account_id}}":      acct.ID,
		"{{condition}}":       condition,
		"{{field}}":           field,
		"{{value}}":           fmt.Sprintf("%.2f", value),
		"{{throttle_status}}": acct.Fields["throttle_status"],
		"{{utilization_5h}}":  acct.Fields["utilization_5h"],
		"{{utilization_7d}}":  acct.Fields["utilization_7d"],
		"{{total_cost_usd}}":  acct.Fields["total_cost_usd"],
		"{{provider}}":        acct.Fields["provider"],
	}

	fields := map[string]string{
		"account_event_key": key,
		"account_name":      acct.Title,
		"account_id":        acct.ID,
		"condition":         condition,
	}

	return TriggerMatch{
		Key:          key,
		Title:        fmt.Sprintf("Account %s: %s", acct.Title, condition),
		Fields:       fields,
		Labels:       []string{"source:account", "account:" + acct.Title},
		TemplateVars: vars,
	}
}

// CatchUpKeys returns dedup keys for existing beads created by this trigger.
func (s *AccountTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + triggerID},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}

	var keys []string
	for _, bead := range result.Beads {
		if key := bead.Fields["account_event_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *AccountTriggerSource) getDaemon() TriggerBeadClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.daemon
}

// compareFloat compares a float value against an operator and threshold.
func compareFloat(val float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return val > threshold
	case ">=":
		return val >= threshold
	case "<":
		return val < threshold
	case "<=":
		return val <= threshold
	case "==":
		return val == threshold
	default:
		return val >= threshold
	}
}
