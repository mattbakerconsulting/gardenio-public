package main

// /cassettes — the Phase-7 S6 observability SURFACE (R5 synthesis §4 Stretch S6,
// epic ka-0000006108, task ka-0000006302). It renders the wire-audit that the
// pure claudemore.AuditCassette extractor (internal/claudemore/audit.go,
// substrate ka-0000003126 / MR !7069) projects out of every recorded cassette.
//
// WHY IT LIVES HERE (R1 §5). claudemore is the single vantage point that sees,
// on the LLM wire, BOTH the native Claude-Code identity the client stamps into
// `metadata.user_id` AND the model's reasoning plaintext. Recording that as a
// redaction-safe cassette turns it into an artifact the viewer can render —
// keyed to NATIVE session identity instead of the "header archaeology" the
// litellm callback resorts to. This is the read-only half of S6; the LIVE
// native-thinking->NATS publisher (part 2) rides the credential-gated record
// path and is a separate follow-up.
//
// COMPOSITION over the merged substrate. The handler does no LLM parsing itself:
// it enumerates cassette refs (claudemore.CassetteLister), fetches each body
// (claudemore.CassetteStore.Get), and hands the bytes to
// claudemore.AuditCassette. Exactly as the S5 drift-scan orchestrator rode on
// top of the pure CompareToLive primitive, this surface rides on top of the pure
// AuditCassette primitive — no new redaction, no cost metering (§5.2 cost fence:
// token counts are "what the model said it used", never a dollar figure).
//
// DEGRADED-MODE by construction. When no cassette store is wired (local dev /
// tests without AWS creds, or a claudeless namespace that records nothing) the
// route registers but renders an explicit empty state — never a 5xx — mirroring
// the /image proxy's 503-when-unwired discipline. Observability is never a gate
// (R5 §5.1).

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	"gardenio/controller/internal/claudemore"
)

// cassetteReader is the read-side seam the /cassettes surface needs: enumerate
// every cassette (CassetteLister) and fetch one by its (model, hash) ref
// (CassetteStore.Get). Both concrete stores — S3CassetteStore and
// FSCassetteStore — satisfy it, and it is the same union DriftScan depends on
// (internal/claudemore/drift_scan.go). Declaring it here (rather than importing
// the unexported one) keeps the viewer's dependency on claudemore to its
// exported surface and lets a test inject an in-memory fake.
type cassetteReader interface {
	claudemore.CassetteLister
	claudemore.CassetteStore
}

// cassetteAuditFetchTimeout bounds the whole page's store I/O. Enumeration plus
// one Get per cassette must complete within this window or the page renders what
// it has and flags the rest — a slow/stuck S3 must never pin the handler. The
// surface is observability, so a partial render beats a hung one.
const cassetteAuditFetchTimeout = 20 * time.Second

// cassetteAuditFetchConcurrency caps in-flight Get calls so a large store does
// not open one S3 connection per cassette at once. Mirrors the bounded-errgroup
// discipline the /plans enrichment uses (ka-0000004530).
const cassetteAuditFetchConcurrency = 8

// cassetteAuditMaxRows caps how many cassettes the page renders in one shot so a
// store with thousands of cassettes cannot blow the page size or the fetch
// budget. When the store holds more, the surface renders the first N (by stable
// ref order) and flags the remainder — a silent truncation would read as
// "these are all the cassettes" when it is not (advice: no silent caps).
const cassetteAuditMaxRows = 200

// cassetteThinkingPreviewChars is how much reasoning plaintext a row shows
// inline. The full text is available in the cassette artifact; the surface is a
// scannable index, not a transcript reader, so it clips and shows the char count
// (ThinkingChars) as the authoritative length.
const cassetteThinkingPreviewChars = 240

// SetCassetteStore wires the read-side cassette store backing /cassettes. Called
// from main when a store is configured (S3 via COOP_S3_BUCKET, or a local dir
// via CLAUDEMORE_CASSETTE_DIR — the same env contract the claudemore proxy
// reads). Nil leaves the page in degraded mode (explicit empty state, never a
// 5xx). Idempotent; main calls it at most once.
func (s *Server) SetCassetteStore(store cassetteReader) {
	s.cassetteStore = store
}

// newCassetteStoreFromEnv builds the read-side cassette store from the SAME env
// contract the claudemore proxy reads (cmd/claudemore/main.go): a filesystem
// store when CLAUDEMORE_CASSETTE_DIR is set (in-box / hermetic), else an S3
// store over COOP_S3_BUCKET (+ COOP_S3_PREFIX / COOP_S3_REGION /
// COOP_S3_ENDPOINT). It returns (nil, nil) — NOT an error — when neither is
// configured, so a viewer without a cassette store simply renders /cassettes in
// degraded mode instead of failing to boot. The filesystem branch takes
// precedence for parity with the proxy's s3client.go, which lets a dev that sets
// both use the local dir deterministically.
func newCassetteStoreFromEnv(ctx context.Context) (cassetteReader, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDEMORE_CASSETTE_DIR")); dir != "" {
		return claudemore.NewFSCassetteStore(dir)
	}
	bucket := strings.TrimSpace(os.Getenv("COOP_S3_BUCKET"))
	if bucket == "" {
		return nil, nil // no store configured — degraded mode, not an error.
	}
	client, err := newCassetteS3Client(ctx)
	if err != nil {
		return nil, err
	}
	return claudemore.NewS3CassetteStore(client, bucket, strings.TrimSpace(os.Getenv("COOP_S3_PREFIX")))
}

// newCassetteS3Client builds an S3 client for the cassette store using the same
// AWS config chain + endpoint/region env vars as the /image proxy's
// newImageProxyS3Client (COOP_S3_REGION / COOP_S3_ENDPOINT). It reuses the image
// proxy's endpoint-option and timeout helpers so the two S3 readers in this
// binary resolve identically — a single place to reason about IRSA/MinIO wiring.
func newCassetteS3Client(ctx context.Context) (*s3.Client, error) {
	loadCtx, cancel := context.WithTimeout(ctx, imageProxyAWSConfigTimeout)
	defer cancel()

	var opts []func(*awsconfig.LoadOptions) error
	if region := strings.TrimSpace(os.Getenv("COOP_S3_REGION")); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(loadCtx, opts...)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, imageProxyS3EndpointOpts()...), nil
}

// cassetteAuditRow is one rendered cassette: the wire-audit projection plus a
// pre-clipped thinking preview the template shows inline. Every field derives
// from claudemore.AuditCassette over an already-redacted cassette, so the row is
// redaction-safe by construction.
type cassetteAuditRow struct {
	claudemore.WireAudit
	// ThinkingPreview is ThinkingText clipped to cassetteThinkingPreviewChars for
	// inline display; ThinkingClipped is true when clipping dropped characters so
	// the template can render an ellipsis + the full ThinkingChars count.
	ThinkingPreview string
	ThinkingClipped bool
	// ToolNames is the ordered list of tool_use block names (ToolUses projected to
	// just the recognizable label) so the template can render the tool-choice
	// audit as a compact chip row without re-walking the ToolUseRef structs.
	ToolNames []string
}

// handleCassettes renders the cassette wire-audit index. It enumerates the
// store, fetches each cassette under a bounded errgroup, projects it through
// claudemore.AuditCassette, and renders one row per cassette sorted by native
// session identity then request hash (a stable, scannable order independent of
// the store's unspecified enumeration order). A missing store, an empty store,
// or an enumeration error each degrade to an explicit non-error empty/notice
// state.
func (s *Server) handleCassettes(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":        "Cassettes",
		"StoreEnabled": s.cassetteStore != nil,
	}
	if s.cassetteStore == nil {
		// Degraded mode: route registered, no store wired. Render the shell with
		// an explanatory empty state rather than a 5xx.
		s.render(w, r, "cassettes.html", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cassetteAuditFetchTimeout)
	defer cancel()

	refs, err := s.cassetteStore.List(ctx)
	if err != nil {
		s.logger.Warn("cassettes: enumerate failed", "error", err)
		data["Error"] = "Could not list cassettes from the store."
		s.render(w, r, "cassettes.html", data)
		return
	}

	// Stable ref order BEFORE the truncation cap so "first N" is deterministic
	// (the store's List order is unspecified) and a re-render shows the same
	// window. Sort by model then hash — a compact key that groups a model's
	// cassettes together.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Model != refs[j].Model {
			return refs[i].Model < refs[j].Model
		}
		return refs[i].Hash < refs[j].Hash
	})

	total := len(refs)
	truncated := false
	if len(refs) > cassetteAuditMaxRows {
		refs = refs[:cassetteAuditMaxRows]
		truncated = true
	}

	rows := s.fetchCassetteAuditRows(ctx, refs)

	// Final display order: by native session identity, then request hash, so a
	// single agent session's exchanges cluster. Cassettes with no metadata
	// (empty SessionUserID) sort last under an empty key.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].SessionUserID != rows[j].SessionUserID {
			return rows[i].SessionUserID < rows[j].SessionUserID
		}
		return rows[i].RequestHash < rows[j].RequestHash
	})

	data["Rows"] = rows
	data["Total"] = total
	data["Shown"] = len(rows)
	data["Truncated"] = truncated
	data["Limit"] = cassetteAuditMaxRows
	// FetchFailures is total-shown-vs-rendered: a cassette that failed to fetch
	// is surfaced as a coverage gap, never silently dropped.
	data["FetchFailures"] = len(refs) - len(rows)
	s.render(w, r, "cassettes.html", data)
}

// fetchCassetteAuditRows fetches every ref's cassette under a bounded errgroup
// and projects each through claudemore.AuditCassette. A ref whose Get fails is
// omitted (logged, counted as a coverage gap by the caller) rather than aborting
// the whole page — a single unreadable cassette must not blank the surface.
func (s *Server) fetchCassetteAuditRows(ctx context.Context, refs []claudemore.CassetteRef) []cassetteAuditRow {
	rows := make([]cassetteAuditRow, len(refs))
	ok := make([]bool, len(refs))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(cassetteAuditFetchConcurrency)
	for i, ref := range refs {
		i, ref := i, ref
		g.Go(func() error {
			c, err := s.cassetteStore.Get(gctx, ref.Model, ref.Hash)
			if err != nil {
				// Don't fail the group — one bad cassette is a coverage gap, not a
				// page failure. Return nil so siblings still render.
				s.logger.Warn("cassettes: fetch failed",
					"model", ref.Model, "hash", ref.Hash, "error", err)
				return nil
			}
			rows[i] = newCassetteAuditRow(claudemore.AuditCassette(c))
			ok[i] = true
			return nil
		})
	}
	// Error is only ctx cancellation here (Get failures are swallowed above); a
	// deadline still yields whatever completed.
	_ = g.Wait()

	out := make([]cassetteAuditRow, 0, len(refs))
	for i := range rows {
		if ok[i] {
			out = append(out, rows[i])
		}
	}
	return out
}

// newCassetteAuditRow decorates a WireAudit with the display-only fields the
// template renders (a clipped thinking preview and the flattened tool-name
// list). It performs no parsing — the audit is already the parsed projection.
func newCassetteAuditRow(a claudemore.WireAudit) cassetteAuditRow {
	row := cassetteAuditRow{WireAudit: a}

	preview := a.ThinkingText
	// Clip on a rune boundary so a multibyte reasoning trace never renders a
	// broken glyph. ThinkingChars (byte length) stays the authoritative size.
	if r := []rune(preview); len(r) > cassetteThinkingPreviewChars {
		preview = strings.TrimSpace(string(r[:cassetteThinkingPreviewChars]))
		row.ThinkingClipped = true
	}
	row.ThinkingPreview = preview

	if len(a.ToolUses) > 0 {
		names := make([]string, 0, len(a.ToolUses))
		for _, t := range a.ToolUses {
			name := strings.TrimSpace(t.Name)
			if name == "" {
				name = "(unnamed)"
			}
			names = append(names, name)
		}
		row.ToolNames = names
	}
	return row
}
