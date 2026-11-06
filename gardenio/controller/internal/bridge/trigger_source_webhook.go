package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// WebhookTriggerSource implements TriggerSource for generic inbound webhook triggers.
// Unlike polling sources, this is event-driven: the HTTP handler buffers incoming
// webhook payloads, and Evaluate returns matches against trigger conditions.
//
// The source identity (name + the trigger label it loads) is configurable so the
// same machinery can back distinct sources: the generic "webhook" source
// (trigger:webhook) and, via NewNamedWebhookTriggerSource, a source scoped to
// another label such as trigger:alert. Scoping each to its own label set lets
// two processes share the HMAC / parse / dedup path without both turning the
// same payload into beads. (The alert-scoped instance was run by
// alertmanager-bridge, deleted 2026-10-31, gd-Y4ZCGr2m2t; history in
// gardenio/docs/research/obs-w3-alertmanager-bridge.md §4.)
type WebhookTriggerSource struct {
	logger *slog.Logger

	// name is the TriggerSource identity (Name()), e.g. "webhook" or "alert".
	name string
	// label is the trigger bead label this source loads, e.g. "trigger:webhook"
	// or "trigger:alert".
	label string

	mu      sync.Mutex
	pending []webhookEvent // buffered events waiting for evaluation
	// triggers is a cache of loaded trigger definitions, keyed by webhook_token.
	// Refreshed on each LoadTriggers call.
	triggers map[string]TriggerDef

	// notify is an optional channel signaled when a new event is buffered.
	// The trigger engine loop can select on this to evaluate immediately
	// instead of waiting for the next poll tick.
	notify chan struct{}
}

// webhookEvent is a buffered inbound webhook payload.
type webhookEvent struct {
	TriggerID string
	Token     string
	Payload   map[string]any
	Headers   map[string]string
}

// webhookMaxBodyBytes bounds inbound webhook request bodies. Trigger payloads
// from SaaS vendors (PagerDuty, GitHub, Alertmanager, ...) are measured in KB;
// 1 MiB is generous. See AUDIT-2026-04-19-jira-gitlab-wl.md §J4.
const webhookMaxBodyBytes = 1 << 20 // 1 MiB

// webhookTriggerData holds webhook-specific fields parsed from a trigger bead.
type webhookTriggerData struct {
	// Token is the unique path token for this trigger's webhook URL.
	Token string
	// Secret is the HMAC-SHA256 secret for signature verification.
	Secret string
	// AllowUnsigned opts the trigger in to accepting requests without HMAC
	// verification. The secure default (zero value) is to require a signature.
	// See AUDIT-2026-04-19-jira-gitlab-wl.md §J3.
	AllowUnsigned bool
	// Conditions is the list of JSONPath conditions that must all match.
	Conditions []webhookCondition
}

// webhookCondition is a single condition evaluated against the webhook payload.
type webhookCondition struct {
	// Path is a dot-separated path into the JSON payload (e.g. "event.type", "alert.severity").
	Path string `json:"path"`
	// Op is the comparison operator: "==", "!=", "in", "not_in", "contains", "present", "absent".
	Op string `json:"op"`
	// Value is the comparison value. For "in"/"not_in", comma-separated list.
	Value string `json:"value"`
}

// NewWebhookTriggerSource creates a new generic webhook trigger source — name
// "webhook", loading trigger:webhook beads.
func NewWebhookTriggerSource(logger *slog.Logger) *WebhookTriggerSource {
	return NewNamedWebhookTriggerSource(logger, "webhook")
}

// NewNamedWebhookTriggerSource creates a webhook trigger source scoped to a
// custom source name. The trigger label it loads is "trigger:<name>", so name
// "alert" loads trigger:alert beads. An empty name falls back to "webhook".
func NewNamedWebhookTriggerSource(logger *slog.Logger, name string) *WebhookTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	if name == "" {
		name = "webhook"
	}
	return &WebhookTriggerSource{
		logger:   logger,
		name:     name,
		label:    "trigger:" + name,
		triggers: make(map[string]TriggerDef),
		notify:   make(chan struct{}, 1),
	}
}

// Notify returns a channel that receives a signal whenever a new webhook
// event is buffered. Use this to trigger immediate evaluation instead of
// waiting for the next poll tick.
func (s *WebhookTriggerSource) Notify() <-chan struct{} {
	return s.notify
}

func (s *WebhookTriggerSource) Name() string { return s.name }

// LoadTriggers fetches all enabled trigger beads carrying this source's label
// ("trigger:webhook" by default, "trigger:alert" for the alert-scoped source).
func (s *WebhookTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{s.label},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}

	triggersByToken := make(map[string]TriggerDef)
	var triggers []TriggerDef
	for _, bead := range result.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}

		token := bead.Fields["webhook_token"]
		if token == "" {
			s.logger.Warn("webhook trigger: missing webhook_token, skipping",
				"trigger", bead.ID)
			continue
		}

		t := ParseTriggerDef(bead, s.name)
		allowUnsigned := bead.Fields["allow_unsigned"] == "true"
		data := webhookTriggerData{
			Token:         token,
			Secret:        bead.Fields["webhook_secret"],
			AllowUnsigned: allowUnsigned,
		}
		if allowUnsigned {
			s.logger.Warn("webhook trigger: allow_unsigned=true — HMAC verification disabled (insecure)",
				"trigger", bead.ID, "token", token)
		} else if data.Secret == "" {
			s.logger.Error("webhook trigger: no webhook_secret and allow_unsigned!=true — handler will reject all requests",
				"trigger", bead.ID, "token", token)
		}

		// Parse conditions from JSON array field.
		if cond := bead.Fields["conditions"]; cond != "" {
			var conditions []webhookCondition
			if err := json.Unmarshal([]byte(cond), &conditions); err != nil {
				s.logger.Warn("webhook trigger: invalid conditions JSON",
					"trigger", bead.ID, "error", err)
				continue
			}
			data.Conditions = conditions
		}

		t.SourceData = data
		triggers = append(triggers, t)
		triggersByToken[token] = t
	}

	// Update the cached trigger map for the HTTP handler.
	s.mu.Lock()
	s.triggers = triggersByToken
	s.mu.Unlock()

	return triggers, nil
}

// Evaluate drains pending webhook events and returns matches for the given trigger.
func (s *WebhookTriggerSource) Evaluate(_ context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(webhookTriggerData)
	if !ok {
		return nil, nil
	}

	s.mu.Lock()
	events := s.pending
	s.mu.Unlock()

	var matches []TriggerMatch
	for _, evt := range events {
		if evt.TriggerID != trig.ID {
			continue
		}

		if !matchWebhookConditions(data.Conditions, evt.Payload) {
			continue
		}

		matches = append(matches, webhookEventToMatch(evt, s.name))
	}
	return matches, nil
}

// DrainPending clears the pending event buffer. Call after all triggers have been
// evaluated in a cycle to prevent unbounded growth.
func (s *WebhookTriggerSource) DrainPending() {
	s.mu.Lock()
	s.pending = s.pending[:0]
	s.mu.Unlock()
}

// PendingCount returns the number of buffered events (for testing/monitoring).
func (s *WebhookTriggerSource) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// CatchUpKeys returns dedup keys for existing beads created by this trigger.
func (s *WebhookTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
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
		if key := bead.Fields["webhook_event_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// HandleWebhook returns an HTTP handler that receives inbound webhook requests.
// The handler routes requests by URL path token to the correct trigger, verifies
// HMAC signatures, parses the JSON payload, and buffers events for evaluation.
//
// Endpoint: POST /webhooks/{token}
func (s *WebhookTriggerSource) HandleWebhook() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Extract token from URL path: /webhooks/{token}
		token := extractWebhookToken(r.URL.Path)
		if token == "" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		// Look up the trigger by token.
		s.mu.Lock()
		trig, ok := s.triggers[token]
		s.mu.Unlock()

		if !ok {
			s.logger.Debug("webhook: unknown token", "token", token)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		data, ok := trig.SourceData.(webhookTriggerData)
		if !ok {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Bound the body read before allocation. An unbounded ReadAll lets an
		// attacker who knows (or guesses, for allow_unsigned triggers) the token
		// drive the bridge into multi-GB allocation via deeply nested JSON.
		// See AUDIT-2026-04-19-jira-gitlab-wl.md §J4.
		r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.logger.Error("webhook: failed to read body", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Require HMAC verification unless the trigger explicitly opts in to
		// unsigned requests. The secure default is fail-closed: an operator who
		// omits webhook_secret must also set allow_unsigned=true to accept any
		// traffic. See AUDIT-2026-04-19-jira-gitlab-wl.md §J3.
		if !data.AllowUnsigned {
			if data.Secret == "" {
				s.logger.Warn("webhook: no secret configured and allow_unsigned!=true, rejecting",
					"trigger", trig.ID, "token", token)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if !verifyWebhookHMAC(body, r.Header, data.Secret) {
				s.logger.Warn("webhook: HMAC verification failed",
					"trigger", trig.ID, "token", token)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		// Parse JSON payload.
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			s.logger.Error("webhook: invalid JSON payload",
				"trigger", trig.ID, "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Capture relevant headers.
		headers := map[string]string{
			"content-type":   r.Header.Get("Content-Type"),
			"user-agent":     r.Header.Get("User-Agent"),
			"x-request-id":   r.Header.Get("X-Request-Id"),
			"x-github-event": r.Header.Get("X-GitHub-Event"),
			// X-GitHub-Delivery is a per-delivery GUID that GitHub keeps across
			// redeliveries; buildWebhookDedupKey falls back to it for GitHub
			// events whose payload carries no recognised object id.
			"x-github-delivery": r.Header.Get("X-GitHub-Delivery"),
		}

		// Buffer the event.
		s.mu.Lock()
		s.pending = append(s.pending, webhookEvent{
			TriggerID: trig.ID,
			Token:     token,
			Payload:   payload,
			Headers:   headers,
		})
		s.mu.Unlock()

		// Signal the trigger engine to evaluate immediately.
		select {
		case s.notify <- struct{}{}:
		default:
		}

		s.logger.Info("webhook: event received",
			"trigger", trig.ID, "token", token)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"accepted","trigger":"%s"}`, trig.ID)
	})
}

// extractWebhookToken extracts the token from a URL path like "/webhooks/{token}".
func extractWebhookToken(path string) string {
	// Normalize: strip trailing slash.
	path = strings.TrimSuffix(path, "/")

	// Handle "/webhooks/TOKEN".
	if after, ok := strings.CutPrefix(path, "/webhooks/"); ok {
		return after
	}
	return ""
}

// verifyWebhookHMAC verifies the HMAC-SHA256 signature of a webhook payload.
// Checks multiple common signature header formats:
//   - X-Hub-Signature-256: sha256=<hex>  (GitHub)
//   - X-Hub-Signature: sha256=<hex>      (Jira, generic)
//   - X-Webhook-Signature: sha256=<hex>  (PagerDuty, custom)
func verifyWebhookHMAC(body []byte, headers http.Header, secret string) bool {
	// Try common signature headers in priority order.
	sigHeaders := []string{
		"X-Hub-Signature-256",
		"X-Hub-Signature",
		"X-Webhook-Signature",
	}

	for _, h := range sigHeaders {
		sig := headers.Get(h)
		if sig == "" {
			continue
		}
		sig = strings.TrimPrefix(sig, "sha256=")
		expected, err := hex.DecodeString(sig)
		if err != nil {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), expected) {
			return true
		}
	}

	return false
}

// matchWebhookConditions checks if all conditions match the webhook payload.
func matchWebhookConditions(conditions []webhookCondition, payload map[string]any) bool {
	for _, c := range conditions {
		if !matchOneWebhookCondition(c, payload) {
			return false
		}
	}
	return true
}

// matchOneWebhookCondition evaluates a single condition against a webhook payload.
func matchOneWebhookCondition(c webhookCondition, payload map[string]any) bool {
	fieldVal := extractJSONPath(payload, c.Path)

	switch c.Op {
	case "==", "eq":
		return fieldVal == c.Value
	case "!=", "ne":
		return fieldVal != c.Value
	case "in":
		for v := range strings.SplitSeq(c.Value, ",") {
			if strings.TrimSpace(v) == fieldVal {
				return true
			}
		}
		return false
	case "not_in":
		for v := range strings.SplitSeq(c.Value, ",") {
			if strings.TrimSpace(v) == fieldVal {
				return false
			}
		}
		return true
	case "contains":
		return strings.Contains(fieldVal, c.Value)
	case "present":
		return fieldVal != ""
	case "absent":
		return fieldVal == ""
	default:
		return false
	}
}

// extractJSONPath extracts a value from a nested JSON object using a dot-separated path.
// For example, "event.type" extracts payload["event"]["type"].
// Returns the string representation of the value, or "" if not found.
func extractJSONPath(obj map[string]any, path string) string {
	if path == "" || obj == nil {
		return ""
	}

	parts := strings.Split(path, ".")
	var current any = obj

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]any:
			val, ok := v[part]
			if !ok {
				return ""
			}
			current = val
		default:
			return ""
		}
	}

	// Convert the final value to string.
	switch v := current.(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// webhookEventToMatch converts a buffered webhook event to a TriggerMatch.
// sourceName stamps the "source:<name>" label so matches from an alert-scoped
// source carry "source:alert" rather than "source:webhook".
func webhookEventToMatch(evt webhookEvent, sourceName string) TriggerMatch {
	if sourceName == "" {
		sourceName = "webhook"
	}
	// Build a dedup key from the payload.
	key := buildWebhookDedupKey(evt)

	// Build template vars from the flattened payload.
	vars := make(map[string]string)
	flattenPayload("", evt.Payload, vars)

	// Add header vars.
	for k, v := range evt.Headers {
		if v != "" {
			vars["{{header_"+k+"}}"] = v
		}
	}
	vars["{{webhook_token}}"] = evt.Token

	fields := map[string]string{
		"webhook_event_key": key,
		"webhook_token":     evt.Token,
	}

	// Extract a title from common payload patterns.
	title := extractWebhookTitle(evt.Payload)

	return TriggerMatch{
		Key:          key,
		Title:        title,
		Fields:       fields,
		Labels:       []string{"source:" + sourceName},
		TemplateVars: vars,
	}
}

// buildWebhookDedupKey creates a unique key for deduplication.
// Tries GitHub object ids, then common ID fields from the payload, then the
// GitHub delivery GUID, and finally falls back to a hash.
//
// GitHub workflow_run / check_suite payloads have no top-level id, so before
// these were keyed explicitly every such event collapsed to
// "<token>:<action>" (e.g. "<token>:completed") and only the first run a
// token ever saw fired (gd-ExCBfoRCTA).
func buildWebhookDedupKey(evt webhookEvent) string {
	if key := githubObjectDedupKey(evt.Payload); key != "" {
		return evt.Token + ":" + key
	}

	// Try common unique ID fields.
	idPaths := []string{
		"id",
		"event.id",
		"message.id",
		"incident.id",
		"alert.id",
		"delivery_id",
		"groupKey", // Alertmanager / VictoriaMetrics
	}

	for _, path := range idPaths {
		if val := extractJSONPath(evt.Payload, path); val != "" {
			return evt.Token + ":" + val
		}
	}

	// Any other GitHub event: the delivery GUID is unique per event and is
	// preserved when GitHub redelivers it, so it beats the coarse "action".
	if d := evt.Headers["x-github-delivery"]; d != "" {
		return evt.Token + ":delivery=" + d
	}

	if val := extractJSONPath(evt.Payload, "action"); val != "" {
		return evt.Token + ":" + val
	}

	// Fallback: hash the entire payload.
	b, _ := json.Marshal(evt.Payload)
	h := sha256.Sum256(b)
	return evt.Token + ":" + hex.EncodeToString(h[:8])
}

// githubObjectDedupKey keys a GitHub workflow_run or check_suite payload on
// the object it describes: "workflow_run:<id>:<run_attempt>[:<action>]" or
// "check_suite:<id>[:<action>]". run_attempt keeps a re-run distinct from the
// attempt it re-runs; action keeps a run's "requested" event from consuming
// the key its "completed" event needs. A redelivery of the same event yields
// the same key. Returns "" for any other payload.
func githubObjectDedupKey(payload map[string]any) string {
	action := extractJSONPath(payload, "action")
	withAction := func(key string) string {
		if action != "" {
			key += ":" + action
		}
		return key
	}
	if id := extractJSONPath(payload, "workflow_run.id"); id != "" {
		attempt := extractJSONPath(payload, "workflow_run.run_attempt")
		if attempt == "" {
			attempt = "1"
		}
		return withAction("workflow_run:" + id + ":" + attempt)
	}
	if id := extractJSONPath(payload, "check_suite.id"); id != "" {
		return withAction("check_suite:" + id)
	}
	return ""
}

// extractWebhookTitle extracts a human-readable title from common webhook payload patterns.
// Supports PagerDuty, Alertmanager/VictoriaMetrics, GitHub, and generic payloads.
func extractWebhookTitle(payload map[string]any) string {
	titlePaths := []string{
		// PagerDuty / generic incident
		"incident.title",
		"incident.summary",
		"alert.summary",
		// Alertmanager / VictoriaMetrics
		"commonAnnotations.summary",
		"commonAnnotations.description",
		"commonLabels.alertname",
		"groupLabels.alertname",
		// Generic event patterns
		"event.title",
		"message.title",
		"message.text",
		"action",
		"event_type",
	}

	for _, path := range titlePaths {
		if val := extractJSONPath(payload, path); val != "" {
			return val
		}
	}

	return "Webhook event"
}

// flattenPayload recursively flattens a JSON object into template vars.
// Keys use the pattern "{{path.to.field}}" (e.g. "{{event.type}}").
func flattenPayload(prefix string, obj map[string]any, out map[string]string) {
	for k, v := range obj {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]any:
			flattenPayload(key, val, out)
		case string:
			out["{{"+key+"}}"] = val
		case float64:
			if val == float64(int64(val)) {
				out["{{"+key+"}}"] = fmt.Sprintf("%d", int64(val))
			} else {
				out["{{"+key+"}}"] = fmt.Sprintf("%g", val)
			}
		case bool:
			if val {
				out["{{"+key+"}}"] = "true"
			} else {
				out["{{"+key+"}}"] = "false"
			}
		case nil:
			// skip nil values
		default:
			b, _ := json.Marshal(val)
			out["{{"+key+"}}"] = string(b)
		}
	}
}
