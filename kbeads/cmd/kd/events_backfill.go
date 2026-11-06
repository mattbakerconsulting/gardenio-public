package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// backfillMetricName is the counter series this replay reconstructs. It MUST
// match metrics.BeadEvents (gardenio_beads_events_total) so the backfilled
// history and the live counter share one series identity.
const backfillMetricName = "gardenio_beads_events_total"

// defaultBackfillWindow is the D8 replay window from the E8 runbook (90 days).
const defaultBackfillWindow = 90 * 24 * time.Hour

// vmImportPath is the VictoriaMetrics Prometheus-exposition import endpoint
// appended to --vm-url.
const vmImportPath = "/api/v1/import/prometheus"

// eventsBackfillCmd drives E4b (ka-0000006538): a best-effort historical
// replay of the durable events table into VictoriaMetrics as
// gardenio_beads_events_total samples, each timestamped at events.created_at.
//
// It pages GET /v1/events over [--since, --until] (chronological, cursor-keyed
// on event id), projects every row onto the SAME bounded {event,project,
// bead_type} schema the live counter uses (metrics.ProjectBeadEventLabel — the
// shared source of truth, so live and backfill never disagree on labeling),
// maintains in-memory per-label-tuple running totals, and pushes the
// cumulative value at each event's created_at via the VM Prometheus-import
// endpoint.
//
// This is a best-effort LOWER BOUND, never an authoritative replay: events
// whose operator could not be resolved are absent from the events table
// (migration-026 fail-closed skip; R4-delta §2.1), yet the live counter — which
// increments unconditionally at recordAndPublish entry — counted them. Do not
// expect, and do not assert, that a backfilled total equals the live counter
// for the same window. Eventbus families (agents/decisions/gate) never land in
// the events table and are therefore out of scope here.
var eventsBackfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Replay the events table into VictoriaMetrics as gardenio_beads_events_total (E4b)",
	Long: `Replays historical bead-domain events from the durable events table into
VictoriaMetrics as the gardenio_beads_events_total counter, each sample
timestamped at the original events.created_at.

Events are paged chronologically from --since to --until and projected onto the
same bounded {event,project,bead_type} label schema as the live counter
(shared projection — they cannot drift). A per-label-tuple running total is
maintained in memory and the cumulative value is pushed at each event's
created_at via the VictoriaMetrics Prometheus-import endpoint
(--vm-url` + vmImportPath + `).

This is a BEST-EFFORT LOWER BOUND, not an authoritative replay: events with an
unresolved operator never reached the events table (migration-026) but were
still counted live, so backfilled totals are a floor. Eventbus families
(agents/decisions/gate) are not in the events table and are not backfilled.

Run as part of E8 with the 90-day D8 window (the default --since). Requires the
HTTP transport (the global events feed is HTTP-only). Use --dry-run to preview
the exposition lines and a summary without writing to VictoriaMetrics.`,
	RunE: runEventsBackfill,
}

func init() {
	eventsBackfillCmd.Flags().String("since", "", "replay events created at-or-after this RFC3339 timestamp (default: 90d before --until)")
	eventsBackfillCmd.Flags().String("until", "", "replay events created at-or-before this RFC3339 timestamp (default: now)")
	eventsBackfillCmd.Flags().String("vm-url", os.Getenv("VM_IMPORT_URL"), "VictoriaMetrics base URL for the Prometheus import endpoint (env VM_IMPORT_URL)")
	eventsBackfillCmd.Flags().Int("page-size", 1000, "events fetched per /v1/events page (server caps at 1000)")
	eventsBackfillCmd.Flags().Int("flush-lines", 5000, "exposition lines buffered per VM import POST")
	eventsBackfillCmd.Flags().Int("max-pages", 0, "cap on number of event pages scanned (0 = no cap)")
	eventsBackfillCmd.Flags().Bool("dry-run", false, "project and count but do not POST to VictoriaMetrics; print a sample of lines")
	eventsCmd.AddCommand(eventsBackfillCmd)
}

func runEventsBackfill(cmd *cobra.Command, _ []string) error {
	sinceStr, _ := cmd.Flags().GetString("since")
	untilStr, _ := cmd.Flags().GetString("until")
	vmURL, _ := cmd.Flags().GetString("vm-url")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	flushLines, _ := cmd.Flags().GetInt("flush-lines")
	maxPages, _ := cmd.Flags().GetInt("max-pages")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	until := time.Now().UTC()
	if untilStr != "" {
		t, err := time.Parse(time.RFC3339, untilStr)
		if err != nil {
			return fmt.Errorf("--until: parse %q as RFC3339: %w", untilStr, err)
		}
		until = t
	}
	since := until.Add(-defaultBackfillWindow)
	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return fmt.Errorf("--since: parse %q as RFC3339: %w", sinceStr, err)
		}
		since = t
	}
	if since.After(until) {
		return fmt.Errorf("--since (%s) must be <= --until (%s)", since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	if pageSize <= 0 {
		pageSize = 1000
	}
	if flushLines <= 0 {
		flushLines = 5000
	}
	if !dryRun && strings.TrimSpace(vmURL) == "" {
		return fmt.Errorf("--vm-url (or VM_IMPORT_URL) is required unless --dry-run is set")
	}

	var sink lineSink
	if dryRun {
		sink = newDryRunSink(os.Stderr)
	} else {
		sink = &vmImportSink{
			url:    strings.TrimRight(vmURL, "/") + vmImportPath,
			client: &http.Client{Timeout: 60 * time.Second},
		}
	}

	bf := &backfiller{
		fetch:      beadsClient.ListGlobalEvents,
		sink:       sink,
		totals:     make(map[string]float64),
		pageSize:   pageSize,
		flushLines: flushLines,
		maxPages:   maxPages,
	}

	ctx := context.Background()
	start := time.Now()
	stats, err := bf.run(ctx, since, until)
	if err != nil {
		return err
	}

	manifest := map[string]any{
		"dry_run":         dryRun,
		"vm_url":          vmURL,
		"since":           since.Format(time.RFC3339),
		"until":           until.Format(time.RFC3339),
		"events_scanned":  stats.eventsScanned,
		"samples_emitted": stats.samplesEmitted,
		"distinct_series": len(bf.totals),
		"pages":           stats.pages,
		"pages_capped":    stats.pagesCapped,
		"last_cursor":     stats.lastCursor,
		"client_wall_ms":  time.Since(start).Milliseconds(),
		"note":            "best-effort lower bound; events with unresolved operators are absent from the events table (migration-026). Not an authoritative replay.",
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}

// backfillStats is the running tally returned from a backfill run.
type backfillStats struct {
	eventsScanned  int
	samplesEmitted int
	pages          int
	pagesCapped    bool
	lastCursor     int64
}

// fetchPageFunc fetches one page of global events. Matches
// client.BeadsClient.ListGlobalEvents so the real client plugs in directly and
// tests can substitute a closure.
type fetchPageFunc func(context.Context, *client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error)

// lineSink consumes exposition lines and flushes them in batches.
type lineSink interface {
	// add appends one fully-formed exposition line (without trailing newline).
	add(line string) error
	// flush writes any buffered lines.
	flush() error
}

// backfiller drives the page→project→emit loop with in-memory running totals.
type backfiller struct {
	fetch      fetchPageFunc
	sink       lineSink
	totals     map[string]float64
	pageSize   int
	flushLines int
	maxPages   int
}

func (b *backfiller) run(ctx context.Context, since, until time.Time) (backfillStats, error) {
	var stats backfillStats
	var cursor int64
	for {
		if b.maxPages > 0 && stats.pages >= b.maxPages {
			stats.pagesCapped = true
			break
		}
		resp, err := b.fetch(ctx, &client.ListGlobalEventsRequest{
			Since:  since,
			Until:  until,
			Cursor: cursor,
			Limit:  b.pageSize,
		})
		if err != nil {
			return stats, fmt.Errorf("fetch events page (cursor=%d): %w", cursor, err)
		}
		stats.pages++
		for _, ev := range resp.Events {
			stats.eventsScanned++
			line, ok := b.project(ev)
			if !ok {
				continue
			}
			if err := b.sink.add(line); err != nil {
				return stats, fmt.Errorf("emit sample: %w", err)
			}
			stats.samplesEmitted++
			if b.flushLines > 0 && stats.samplesEmitted%b.flushLines == 0 {
				if err := b.sink.flush(); err != nil {
					return stats, fmt.Errorf("flush samples: %w", err)
				}
			}
			stats.lastCursor = ev.ID
		}
		if !resp.HasMore {
			break
		}
		next, err := strconv.ParseInt(resp.NextCursor, 10, 64)
		if err != nil || next <= cursor {
			// Defensive: a non-advancing or unparseable cursor would loop
			// forever. Stop rather than spin.
			return stats, fmt.Errorf("invalid next_cursor %q after cursor %d", resp.NextCursor, cursor)
		}
		cursor = next
	}
	if err := b.sink.flush(); err != nil {
		return stats, fmt.Errorf("final flush: %w", err)
	}
	return stats, nil
}

// project converts one events-table row into a cumulative-counter exposition
// line, advancing the in-memory running total for its label tuple. The bool is
// false only for an event whose payload fails to decode (skipped, not fatal).
func (b *backfiller) project(ev *model.Event) (string, bool) {
	project, beadType, changes := decodeEventPayload(ev.Payload)
	eventLabel := metrics.ProjectBeadEventLabel(ev.Topic, changes)
	// Normalize project/bead_type to the SAME bounded label values the live
	// ObserveBeadEvent uses, so the series identity matches exactly.
	projLabel := metrics.NormalizeProjectLabel(project)
	typeLabel := metrics.NormalizeBeadType(beadType)

	key := eventLabel + "\x00" + projLabel + "\x00" + typeLabel
	b.totals[key]++
	total := b.totals[key]

	return formatSample(backfillMetricName, eventLabel, projLabel, typeLabel, total, ev.CreatedAt), true
}

// payloadView is the generic decode target for an events.* payload. Every
// bead-carrying payload (created/updated/closed/transferred/advice/jack)
// embeds {"bead": {...}}; updates also embed {"changes": {...}}. Bead-less
// payloads (dependency/label/comment/follower) simply leave Bead nil.
type payloadView struct {
	Bead    *model.Bead    `json:"bead"`
	Changes map[string]any `json:"changes"`
}

// decodeEventPayload extracts the project label source, bead type, and the
// update Changes map from a stored event payload. A nil/empty/garbage payload
// degrades gracefully to ("", "", nil) — which projects to
// project="unknown", bead_type="other", and (for updates) the generic
// "updated" sub-type.
func decodeEventPayload(raw json.RawMessage) (project, beadType string, changes map[string]any) {
	if len(raw) == 0 {
		return "", "", nil
	}
	var v payloadView
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", nil
	}
	if v.Bead != nil {
		project = projectFromLabelSlice(v.Bead.Labels)
		beadType = string(v.Bead.Type)
	}
	return project, beadType, v.Changes
}

// projectFromLabelSlice extracts the project name from labels like
// "project:gardenio", mirroring server.projectFromLabels. Returns "" when no
// project label is present (normalized to "unknown" downstream).
func projectFromLabelSlice(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			return v
		}
	}
	return ""
}

// formatSample renders one VictoriaMetrics Prometheus-import line:
//
//	gardenio_beads_events_total{event="…",project="…",bead_type="…"} <value> <ts_ms>
//
// The label set and ordering match metrics.BeadEvents. The timestamp is the
// event's created_at in epoch milliseconds, so the sample lands at the moment
// the event actually occurred.
func formatSample(name, event, project, beadType string, value float64, ts time.Time) string {
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	sb.WriteString(`event="`)
	sb.WriteString(escapeLabelValue(event))
	sb.WriteString(`",project="`)
	sb.WriteString(escapeLabelValue(project))
	sb.WriteString(`",bead_type="`)
	sb.WriteString(escapeLabelValue(beadType))
	sb.WriteString(`"} `)
	sb.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	sb.WriteByte(' ')
	sb.WriteString(strconv.FormatInt(ts.UnixMilli(), 10))
	return sb.String()
}

// escapeLabelValue escapes a Prometheus exposition label value (backslash,
// double-quote, newline). The bounded label values produced by the metrics
// normalizers never actually contain these, but escaping keeps the writer
// correct if a future label widens.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// vmImportSink batches exposition lines and POSTs them to the VictoriaMetrics
// Prometheus-import endpoint.
type vmImportSink struct {
	url    string
	client *http.Client
	buf    bytes.Buffer
	lines  int
}

func (s *vmImportSink) add(line string) error {
	s.buf.WriteString(line)
	s.buf.WriteByte('\n')
	s.lines++
	return nil
}

func (s *vmImportSink) flush() error {
	if s.lines == 0 {
		return nil
	}
	body := s.buf.Bytes()
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: status %d: %s", s.url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	s.buf.Reset()
	s.lines = 0
	return nil
}

// dryRunSink discards lines but prints a bounded sample and a per-series tally
// to the given writer so an operator can eyeball the projection without
// writing to VictoriaMetrics.
type dryRunSink struct {
	w        io.Writer
	shown    int
	maxShown int
	series   map[string]int
}

func newDryRunSink(w io.Writer) *dryRunSink {
	return &dryRunSink{w: w, maxShown: 20, series: make(map[string]int)}
}

func (s *dryRunSink) add(line string) error {
	if s.shown < s.maxShown {
		fmt.Fprintln(s.w, line)
		s.shown++
		if s.shown == s.maxShown {
			fmt.Fprintln(s.w, "… (further sample lines suppressed)")
		}
	}
	// Tally distinct series from the brace-delimited label set.
	if i := strings.IndexByte(line, '}'); i > 0 {
		s.series[line[:i+1]]++
	}
	return nil
}

func (s *dryRunSink) flush() error {
	if len(s.series) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.series))
	for k := range s.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(s.w, "\n[dry-run] %d distinct series:\n", len(keys))
	for _, k := range keys {
		fmt.Fprintf(s.w, "  %s  ×%d\n", k, s.series[k])
	}
	return nil
}
