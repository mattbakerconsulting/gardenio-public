// Package vlogs is the READ-side counterpart to the pod-log shippers
// (internal/bridgeshipper, internal/traefikshipper, internal/k3sauditshipper,
// all writing through internal/vlsink): where those STREAM gardenio-pod slog
// JSON INTO the in-chart VictoriaLogs store (gardenio-victoria-logs:9428
// /insert/jsonline), this package QUERIES it back
// OUT over /select/logsql/query. It is the thin, bounded, allowlisted LogsQL
// adapter behind the `gb logs <component>` verb (design ka-0000005579 option A,
// task ka-0000005209).
//
// # Why this exists
//
// The gardenio-agent SA is deliberately locked out of the k8s pod-log API
// (ka-0000001752): `kubectl auth can-i get pods/log` = no. That lockdown stays
// fully intact here — this client reads the log STORE over HTTP, it never
// touches Kubernetes. Agents get a governed gb verb; no new pod-log RBAC is
// granted to the agent OR the query path.
//
// # The contract the shippers establish
//
// Every shipper stamps a low-cardinality `source` stream field naming the
// logical component (bridgeshipper's fieldSource; the jsonline ingest URL sets
// _stream_fields=source, _time_field=time, _msg_field=msg). So a per-component
// read is an exact stream selector `{source="<component>"}`, the `--since`
// window is a LogsQL `_time:<dur>` filter, and `--grep` is an exact-phrase word
// filter — all three server-side so the daemon never pulls an unbounded window.
//
// # Bounding (design §5, the security gate)
//
//   - Component ALLOWLIST: only the components in componentSelectors are
//     queryable — never an arbitrary stream selector, pod, or namespace.
//   - Mandatory BOUNDED --since: clamped to [MinSince, MaxSince]; a zero/omitted
//     window defaults to DefaultSince, never "everything".
//   - Result-size CAP: Limit is clamped to [1, MaxLimit] and also sent as the VL
//     `limit` param, so the store bounds the result server-side too.
//   - --grep is quoted as an exact LogsQL phrase, so it can never inject raw
//     LogsQL (no unbounded passthrough query surface).
package vlogs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// componentSelectors is the query allowlist: logical component name → the
// `source` stream-field value the shipper stamps onto that component's records.
// A component that is not a key here is NOT queryable — the daemon refuses it
// rather than translating an arbitrary selector (design §5, "per-component
// allowlist; never arbitrary pods/namespaces"). The four core components
// (controller / slack-bridge / trigger-engine / grafana-bridge) are the debug
// targets the root cause cites; jira-bridge / gitlab-bridge are the two webhook
// doors the existing bridge-log-shipper already ships. All values are their own
// name (the shipper stamps source=<name>); the map is kept explicit so adding a
// queryable component is a deliberate, reviewable edit.
var componentSelectors = map[string]string{
	"controller":     "controller",
	"slack-bridge":   "slack-bridge",
	"trigger-engine": "trigger-engine",
	"grafana-bridge": "grafana-bridge",
	"jira-bridge":    "jira-bridge",
	"gitlab-bridge":  "gitlab-bridge",
}

// Bounds on a query. These are the server-side caps design §5 requires; the CLI
// surfaces them as flag defaults but the client re-clamps so the daemon path is
// authoritative even if a caller passes an out-of-range value directly.
const (
	// MinSince / MaxSince bound the --since window. A window under a minute is
	// pointless churn; a window over a day is the unbounded-read risk the gate
	// forbids. DefaultSince is the "typical debug" window when --since is omitted.
	MinSince     = time.Minute
	MaxSince     = 24 * time.Hour
	DefaultSince = 15 * time.Minute

	// MaxLimit / DefaultLimit bound the result-size cap. VictoriaLogs also honors
	// the `limit` param, so the store bounds the response even before we truncate.
	MaxLimit     = 1000
	DefaultLimit = 200

	// defaultTimeout bounds the whole query. A bounded LogsQL read is fast; this
	// is generous headroom, not a tuning knob.
	defaultTimeout = 30 * time.Second

	// maxLineBytes caps a single scanned response line so one pathological record
	// can't blow the scanner buffer (mirrors the shipper's defaultMaxLine).
	maxLineBytes = 1024 * 1024
)

// Components returns the sorted list of queryable component names — the source
// of truth behind the CLI's arg validation and its help/error text.
func Components() []string {
	out := make([]string, 0, len(componentSelectors))
	for c := range componentSelectors {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// IsAllowed reports whether component is in the query allowlist.
func IsAllowed(component string) bool {
	_, ok := componentSelectors[component]
	return ok
}

// QueryOptions is one bounded, allowlisted log query.
type QueryOptions struct {
	// Component is the logical component to read; must be in the allowlist.
	Component string
	// Since is the look-back window, clamped to [MinSince, MaxSince]. Zero uses
	// DefaultSince.
	Since time.Duration
	// Grep, when non-empty, keeps only records containing this exact phrase
	// (a LogsQL phrase filter over the _msg field and other tokens).
	Grep string
	// Limit caps the number of returned lines, clamped to [1, MaxLimit]. Zero
	// uses DefaultLimit.
	Limit int
}

// normalize validates Component against the allowlist and clamps Since/Limit to
// their bounds, returning the effective values. A disallowed component is a hard
// error naming the allowlist (the daemon refuses it — it never queries an
// arbitrary selector).
func (o QueryOptions) normalize() (source string, since time.Duration, limit int, err error) {
	source, ok := componentSelectors[o.Component]
	if !ok {
		return "", 0, 0, fmt.Errorf("component %q is not queryable; allowed: %s",
			o.Component, strings.Join(Components(), ", "))
	}
	since = o.Since
	switch {
	case since <= 0:
		since = DefaultSince
	case since < MinSince:
		since = MinSince
	case since > MaxSince:
		since = MaxSince
	}
	limit = o.Limit
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}
	return source, since, limit, nil
}

// BuildLogsQL renders the bounded LogsQL for opts: an exact stream selector on
// the shipper-stamped `source` field, a `_time:<dur>` window, and — when set —
// an exact-phrase `--grep` filter. The grep value is quote-escaped into a LogsQL
// phrase so it can only ever narrow the result, never inject raw query syntax.
//
// Exported so the CLI can echo the exact query it ran (auditability) and so the
// translation is unit-testable without an HTTP round trip.
func BuildLogsQL(opts QueryOptions) (string, error) {
	source, since, _, err := opts.normalize()
	if err != nil {
		return "", err
	}
	// `_time:<dur>` is LogsQL's relative-window filter (last <dur>); the stream
	// selector `{source="X"}` restricts to the one component's stream.
	q := fmt.Sprintf(`{source=%s} _time:%s`, logsqlQuote(source), logsqlDuration(since))
	if g := strings.TrimSpace(opts.Grep); g != "" {
		q += " " + logsqlPhrase(g)
	}
	return q, nil
}

// Line is one returned log record. Time/Msg/Source are the partition fields the
// shipper stamps; Raw carries the full record so a caller can render any field.
type Line struct {
	Time   string         `json:"time"`
	Msg    string         `json:"msg"`
	Source string         `json:"source"`
	Raw    map[string]any `json:"-"`
}

// The VictoriaLogs default tenant, which holds every component log. Agent-facing
// readers query only this tenant.
const (
	DefaultAccountID = "0"
	DefaultProjectID = "0"
)

// Client queries a VictoriaLogs store's /select/logsql/query endpoint. Endpoint
// is the base URL (scheme://host:port); the client appends the select path.
type Client struct {
	Endpoint   string
	HTTPClient *http.Client
}

// New builds a Client with a default-timeout HTTP client when one is not
// supplied. endpoint is the VictoriaLogs base URL (no path).
func New(endpoint string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), HTTPClient: hc}
}

// Query runs one bounded, allowlisted read and returns at most Limit lines. It
// normalizes/validates opts (disallowed component → error), POSTs the LogsQL to
// /select/logsql/query, and parses the newline-delimited JSON response, capping
// the parsed lines at the effective limit as belt-and-suspenders to VL's own
// `limit` param.
func (c *Client) Query(ctx context.Context, opts QueryOptions) ([]Line, error) {
	_, _, limit, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	q, err := BuildLogsQL(opts)
	if err != nil {
		return nil, err
	}
	if c.Endpoint == "" {
		return nil, fmt.Errorf("no VictoriaLogs endpoint configured")
	}

	form := url.Values{}
	form.Set("query", q)
	form.Set("limit", fmt.Sprintf("%d", limit))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Endpoint+"/select/logsql/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Pin the default tenant. Records in any other tenant (the k3s API audit log
	// is written to its own, gd-ZrPUxj8wyU) stay out of every agent-facing read,
	// including one whose query is crafted to match them.
	req.Header.Set("AccountID", DefaultAccountID)
	req.Header.Set("ProjectID", DefaultProjectID)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying VictoriaLogs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("VictoriaLogs /select returned %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}
	return parseResponse(resp.Body, limit)
}

// parseResponse reads VictoriaLogs' newline-delimited JSON response into Lines,
// capping at limit. A malformed line is skipped (never a hard failure on one bad
// record); Time/Msg/Source are lifted from the stamped fields and the whole
// record is kept in Raw.
func parseResponse(r io.Reader, limit int) ([]Line, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var out []Line
	for sc.Scan() && len(out) < limit {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(b, &rec); err != nil {
			continue
		}
		out = append(out, Line{
			Time:   stringField(rec, "_time", "time"),
			Msg:    stringField(rec, "_msg", "msg"),
			Source: stringField(rec, "source"),
			Raw:    rec,
		})
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("reading VictoriaLogs response: %w", err)
	}
	return out, nil
}

// stringField returns the first key present as a string, else "".
func stringField(rec map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := rec[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// logsqlQuote wraps a value in double quotes and escapes `\` and `"` so a stream
// selector value is always a valid LogsQL quoted token. Allowlisted source
// values are simple, but the quoting keeps the selector well-formed regardless.
func logsqlQuote(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// logsqlPhrase renders --grep as an exact-phrase LogsQL word filter: the value
// is quoted (escaping `\` and `"`) so it matches the phrase verbatim and cannot
// inject raw LogsQL operators. This is the "translate/validate the query, no raw
// unbounded passthrough" mitigation from design §5.
func logsqlPhrase(v string) string {
	return logsqlQuote(v)
}

// logsqlDuration renders a look-back window for LogsQL `_time:<dur>`. LogsQL
// accepts Go-style compound durations (e.g. "15m", "1h30m", "2h"), so we build
// the string from whole h/m/s components — this avoids time.Duration.String's
// redundant zero units ("15m0s", "1h0m0s") without the fragile suffix-trimming
// that would mangle values like "30m".
func logsqlDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}
