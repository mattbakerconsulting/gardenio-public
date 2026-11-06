package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// Bundles (ka-0000008153, T3) — see plans/agent-artifact-bundles-multi-doc-viewer.md.
// `gb bundle` curates a `type=bundle` parent bead and links one or more
// `type=doc` beads under it parent-child, mirroring the rich-report pattern.
//
// Bundle kinds and doc formats match the schemas defined in
// kbeads/internal/server/config.go (T1 — commit d3051f9e).

var validBundleKinds = map[string]bool{
	"plan":        true,
	"post-mortem": true,
	"spec":        true,
	"report":      true,
	"other":       true,
}

var validDocFormats = map[string]bool{
	"md":    true,
	"diff":  true,
	"csv":   true,
	"code":  true,
	"image": true,
	"link":  true,
	"html":  true,
}

// inlineDocCap matches the soft cap documented on the type=doc schema. Larger
// payloads must be uploaded out-of-band and referenced via fields.content_url.
const inlineDocCap = 256 * 1024

var bundleCmd = &cobra.Command{
	Use:     "bundle",
	Short:   "Curate artifact bundles (parent bead + ordered docs)",
	GroupID: "orchestration",
	Long: `Curate a 'type=bundle' parent bead and the 'type=doc' artifacts under it.

Bundles aggregate the long-form deliverables an agent produces for a task into
a single navigable reading surface at /bundle/<id>. See
plans/agent-artifact-bundles-multi-doc-viewer.md.

Subcommands:
  create  Create a bundle parented to an origin bead
  add     Add an existing bead, or a doc from a path, to a bundle
  update  Rewrite an existing doc's body in place (alias: edit) — no fork
  remove  Detach a doc from a bundle (alias: detach) so it leaves the TOC
  order   Set the position of a doc within a bundle
  locate  Find the bundle(s) attached to an origin/plan bead (reverse lookup)
  show    Print the bundle's table of contents and viewer URL
  doc     Print the rendered body of a type=doc bead`,
}

// ── gb bundle create ───────────────────────────────────────────────────

var bundleCreateCmd = &cobra.Command{
	Use:   "create <origin-bead-id>",
	Short: "Create a bundle parented to an origin bead",
	Long: `Create a 'type=bundle' bead parented to an origin bead.

The new bundle is linked to the origin via a parent-child dependency
(bundle is the child of the origin task), so the origin's bead-show page
can surface it via the typed-mention graph.

Example:
  gb bundle create ka-Origin123 --title "Auth refactor deliverables" --kind plan`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		originID := args[0]
		title, _ := cmd.Flags().GetString("title")
		kind, _ := cmd.Flags().GetString("kind")
		headline, _ := cmd.Flags().GetString("headline")
		summary, _ := cmd.Flags().GetString("summary")

		if title == "" {
			return fmt.Errorf("--title is required")
		}
		if kind == "" {
			kind = "report"
		}
		if !validBundleKinds[kind] {
			return fmt.Errorf("invalid --kind %q (allowed: plan, post-mortem, spec, report, other)", kind)
		}

		// Verify the origin bead exists so we surface a clear error early.
		if _, err := daemon.GetBead(cmd.Context(), originID); err != nil {
			return fmt.Errorf("fetching origin %s: %w", originID, err)
		}

		fields := map[string]any{"kind": kind}
		if headline != "" {
			fields["headline"] = headline
		}
		if summary != "" {
			fields["summary"] = summary
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		bundleID, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     title,
			Type:      "bundle",
			Kind:      "issue",
			Priority:  2,
			CreatedBy: actor,
			Fields:    fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating bundle bead: %w", err)
		}

		// bundle is the child of the origin task: the origin owns the bundle.
		if err := daemon.AddDependency(cmd.Context(), bundleID, originID, "parent-child", actor); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to link bundle to origin: %v\n", err)
		}

		viewerURL := bundleURL(bundleID)
		if jsonOutput {
			printJSON(map[string]string{
				"id":     bundleID,
				"origin": originID,
				"kind":   kind,
				"url":    viewerURL,
			})
		} else {
			fmt.Printf("Bundle %s created (kind=%s, origin=%s)\n", bundleID, kind, originID)
			fmt.Printf("  %s\n", viewerURL)
			// A fresh bundle has zero docs, so that URL currently renders
			// "No documents attached to this bundle yet". Say so at the point
			// the link is minted — an agent that shares it now shares an empty
			// page, and neither the bundle page nor `kd create --type=doc`
			// says anything is wrong (ka-0000000755).
			fmt.Printf("  EMPTY until you attach docs — that URL renders \"No documents attached\" right now.\n")
			fmt.Printf("  Attach: gb bundle add %s --doc <path>   (creates + links + closes the doc bead)\n", bundleID)
			fmt.Printf("  Note: `kd create --type=doc` does NOT attach — it auto-parents to your task, not this bundle.\n")
			fmt.Printf("  Verify before sharing: gb bundle show %s\n", bundleID)
		}
		return nil
	},
}

// ── gb bundle add ──────────────────────────────────────────────────────

var bundleAddCmd = &cobra.Command{
	Use:   "add <bundle-id> [doc-bead-id]",
	Short: "Add a bead, or a doc from a path, to a bundle",
	Long: `Add an existing bead, or a doc created from a local file, to a bundle.

Two forms:

  gb bundle add <bundle-id> <doc-bead-id>
      Link an existing bead under the bundle parent-child.

  gb bundle add <bundle-id> --doc <path> [--format md|diff|csv|code|image|link|html]
      Read the file at <path>, create a 'type=doc' bead with fields.content,
      and link it under the bundle. Files larger than 256 KiB must be uploaded
      out-of-band and referenced with --content-url instead of --doc.

Format defaults are derived from the file extension when --format is omitted.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		bundleID := args[0]
		docPath, _ := cmd.Flags().GetString("doc")
		contentURL, _ := cmd.Flags().GetString("content-url")
		format, _ := cmd.Flags().GetString("format")
		headline, _ := cmd.Flags().GetString("headline")
		title, _ := cmd.Flags().GetString("title")
		language, _ := cmd.Flags().GetString("language")
		position, _ := cmd.Flags().GetInt("position")

		if docPath == "" && contentURL == "" && len(args) < 2 {
			return fmt.Errorf("provide a doc bead id, or --doc <path>, or --content-url <url>")
		}
		if (docPath != "" || contentURL != "") && len(args) >= 2 {
			return fmt.Errorf("cannot combine a doc bead id with --doc/--content-url")
		}

		// Verify the bundle exists.
		bundle, err := daemon.GetBead(cmd.Context(), bundleID)
		if err != nil {
			return fmt.Errorf("fetching bundle %s: %w", bundleID, err)
		}
		if bundle.Type != "bundle" {
			return fmt.Errorf("%s is not a bundle (type=%s)", bundleID, bundle.Type)
		}

		// Path A: link an existing bead.
		if len(args) == 2 {
			docID := args[1]
			if _, err := daemon.GetBead(cmd.Context(), docID); err != nil {
				return fmt.Errorf("fetching doc bead %s: %w", docID, err)
			}
			if err := daemon.AddDependency(cmd.Context(), docID, bundleID, "parent-child", actor); err != nil {
				return fmt.Errorf("linking %s under bundle %s: %w", docID, bundleID, err)
			}
			if position > 0 {
				// position is an integer-typed schema field; the daemon
				// validator rejects a string value with HTTP 400
				// "position: must be an integer". Send it typed (ka-0000005409).
				if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), docID, map[string]any{
					"position": position,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to set position: %v\n", err)
				}
			}
			if jsonOutput {
				printJSON(map[string]string{"bundle": bundleID, "doc": docID})
			} else {
				fmt.Printf("Linked %s under bundle %s\n", docID, bundleID)
				fmt.Printf("  %s\n", bundleURL(bundleID))
			}
			return nil
		}

		// Path B: create a new doc bead.
		fields := map[string]any{}
		var docTitle string

		if docPath != "" {
			info, err := os.Stat(docPath)
			if err != nil {
				return fmt.Errorf("reading --doc path: %w", err)
			}
			if info.IsDir() {
				return fmt.Errorf("--doc must be a file, not a directory")
			}
			data, err := os.ReadFile(docPath)
			if err != nil {
				return fmt.Errorf("reading --doc path: %w", err)
			}
			if int64(len(data)) > inlineDocCap {
				return fmt.Errorf("file %s is %d bytes (>256 KiB cap); upload out-of-band and pass --content-url", docPath, len(data))
			}
			if format == "" {
				format = formatFromExt(docPath)
			}
			fields["content"] = string(data)
			docTitle = filepath.Base(docPath)
		}

		if contentURL != "" {
			if format == "" {
				format = "link"
			}
			fields["content_url"] = contentURL
			if docTitle == "" {
				docTitle = contentURL
			}
		}

		if format == "" {
			return fmt.Errorf("--format is required when neither --doc nor --content-url infers it")
		}
		if !validDocFormats[format] {
			return fmt.Errorf("invalid --format %q (allowed: md, diff, csv, code, image, link, html)", format)
		}
		fields["format"] = format

		if headline != "" {
			fields["headline"] = headline
		}
		if language != "" {
			fields["language"] = language
		}
		if position > 0 {
			fields["position"] = position
		}
		if title != "" {
			docTitle = title
		}
		if docTitle == "" {
			docTitle = fmt.Sprintf("doc (%s) for %s", format, bundleID)
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		// Inject a typed parent mention so the references extractor picks
		// up the doc → bundle edge in `bead_references` alongside the graph
		// dep written below. Keeps `GET /v1/beads/<bundle>/mentioned-by` in
		// sync with the graph for legacy readers (ka-0000003477).
		docID, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:       docTitle,
			Type:        "doc",
			Kind:        "data",
			Priority:    2,
			CreatedBy:   actor,
			Description: "parent(" + bundleID + ")",
			Fields:      fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating doc bead: %w", err)
		}

		if err := daemon.AddDependency(cmd.Context(), docID, bundleID, "parent-child", actor); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to link doc to bundle: %v\n", err)
		}

		if err := daemon.CloseBead(cmd.Context(), docID, nil); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close doc bead: %v\n", err)
		}

		if jsonOutput {
			printJSON(map[string]string{
				"id":     docID,
				"bundle": bundleID,
				"format": format,
				"url":    bundleURL(bundleID) + "#doc-" + docID,
			})
		} else {
			fmt.Printf("Doc %s added to bundle %s (format=%s)\n", docID, bundleID, format)
			fmt.Printf("  %s#doc-%s\n", bundleURL(bundleID), docID)
		}
		return nil
	},
}

// ── gb bundle update ────────────────────────────────────────────────────

// bundleUpdateCmd is the converge verb: it rewrites an EXISTING doc bead's body
// rather than creating a new one. Keeping a plan bundle converged in place is a
// per-cycle plan-gardener job, but before this shipped the only edit path was
// `kd update <doc-id> -f content=…` — reaching under the bundle abstraction —
// and gardeners who instead reran `add --doc` silently forked a duplicate doc
// into the TOC (ka-0000003568, outcome ka-0000005218).
var bundleUpdateCmd = &cobra.Command{
	Use:     "update <doc-id> --doc <path>",
	Aliases: []string{"edit"},
	Short:   "Rewrite an existing doc's body in place (no new doc bead)",
	Long: `Rewrite the body of a doc that is ALREADY in a bundle, in place.

This is the converge verb. 'gb bundle add --doc <path>' always CREATES a new
doc bead, so re-running it against an updated file FORKS a second copy into the
TOC — the duplicate-doc outcome that makes a plan bundle stop reading as a
single plan-of-record. Use this instead when the doc bead already exists and you
want its content replaced: same doc id, same TOC entry, same position, new body.

The same 256 KiB inline cap as 'add' applies; over-cap payloads must be
uploaded out-of-band and referenced with --content-url.

Switching a doc between inline and out-of-band clears the field it is leaving
behind (--doc drops a stale content_url, --content-url drops a stale content),
so the viewer never renders a mix of an old inline body and a new URL.

--format defaults to the doc's CURRENT format — passing a differently-suffixed
file does not silently reclassify it; pass --format explicitly to change it.

A body identical to what is already stored is reported as "unchanged" and
writes nothing, so a per-cycle converge loop is idempotent and does not spam
the activity feed.

Example:
  gb bundle update ka-Doc456 --doc plan.md              # replace the body
  gb bundle edit   ka-Doc456 --doc plan.md --headline "Status: phase 3"
  gb bundle update ka-Doc456 --content-url https://…    # switch to out-of-band`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		docID := args[0]
		docPath, _ := cmd.Flags().GetString("doc")
		contentURL, _ := cmd.Flags().GetString("content-url")
		format, _ := cmd.Flags().GetString("format")
		title, _ := cmd.Flags().GetString("title")
		language, _ := cmd.Flags().GetString("language")
		headlineSet := cmd.Flags().Changed("headline")
		headline, _ := cmd.Flags().GetString("headline")

		if docPath != "" && contentURL != "" {
			return fmt.Errorf("pass --doc or --content-url, not both")
		}
		if docPath == "" && contentURL == "" && !headlineSet &&
			format == "" && title == "" && language == "" {
			return fmt.Errorf("nothing to update: pass --doc <path>, --content-url <url>, --headline, --format, --title, or --language")
		}

		// Verify the target is a doc bead, so a bundle id (or any typo'd id)
		// fails clearly instead of stamping a `content` field onto an
		// unrelated bead. The read also gives us the current format/content to
		// default against.
		doc, err := daemon.GetBead(cmd.Context(), docID)
		if err != nil {
			return fmt.Errorf("fetching doc %s: %w", docID, err)
		}
		if doc.Type != "doc" {
			if doc.Type == "bundle" {
				return fmt.Errorf("%s is a bundle, not a doc — pass a doc id from `gb bundle show %s`", docID, docID)
			}
			return fmt.Errorf("%s is not a doc (type=%s)", docID, doc.Type)
		}

		// changed accumulates "at least one value actually differs from what is
		// stored". Every branch below ORs into it, so a converge loop that
		// re-pushes an identical file reports "unchanged" and writes nothing —
		// a flag that only SOME branches reset would misreport a no-op
		// --headline as an edit.
		fields := map[string]any{}
		changed := false

		if docPath != "" {
			info, err := os.Stat(docPath)
			if err != nil {
				return fmt.Errorf("reading --doc path: %w", err)
			}
			if info.IsDir() {
				return fmt.Errorf("--doc must be a file, not a directory")
			}
			data, err := os.ReadFile(docPath)
			if err != nil {
				return fmt.Errorf("reading --doc path: %w", err)
			}
			if int64(len(data)) > inlineDocCap {
				return fmt.Errorf("file %s is %d bytes (>256 KiB cap); upload out-of-band and pass --content-url", docPath, len(data))
			}
			fields["content"] = string(data)
			changed = changed || string(data) != doc.Fields["content"]
			// A doc cannot be both inline and out-of-band: drop a stale
			// content_url so the viewer cannot render the new inline body
			// alongside the old URL. Only sent when one is actually stored, so
			// the common inline→inline rewrite stays a single-key patch.
			if doc.Fields["content_url"] != "" {
				fields["content_url"] = nil
				changed = true
			}
		}

		if contentURL != "" {
			fields["content_url"] = contentURL
			changed = changed || contentURL != doc.Fields["content_url"]
			if doc.Fields["content"] != "" {
				fields["content"] = nil
				changed = true
			}
		}

		if format != "" {
			if !validDocFormats[format] {
				return fmt.Errorf("invalid --format %q (allowed: md, diff, csv, code, image, link, html)", format)
			}
			fields["format"] = format
			changed = changed || format != doc.Fields["format"]
		}
		if headlineSet {
			// An explicit empty --headline clears the TOC override (the viewer
			// then falls back to the doc title), so gate on Changed() rather
			// than on a non-empty value.
			fields["headline"] = headline
			changed = changed || headline != doc.Fields["headline"]
		}
		if language != "" {
			fields["language"] = language
			changed = changed || language != doc.Fields["language"]
		}

		if len(fields) > 0 {
			if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), docID, fields); err != nil {
				return fmt.Errorf("updating doc %s: %w", docID, err)
			}
		}

		if title != "" && title != doc.Title {
			if err := daemon.UpdateBead(cmd.Context(), docID, beadsapi.UpdateBeadRequest{Title: &title}); err != nil {
				return fmt.Errorf("updating title on doc %s: %w", docID, err)
			}
			changed = true
		}

		// Locate the owning bundle so the printed link lands on the doc's
		// anchor in the viewer. A loose doc (no bundle parent) is NOT an error
		// — editing one is harmless — so a missing parent just drops the URL.
		bundleID := ""
		if deps, err := daemon.GetDependencies(cmd.Context(), docID); err == nil {
			for _, d := range deps {
				if d.Type != "parent-child" {
					continue
				}
				if parent, err := daemon.GetBead(cmd.Context(), d.DependsOnID); err == nil && parent.Type == "bundle" {
					bundleID = parent.ID
					break
				}
			}
		}

		effFormat := format
		if effFormat == "" {
			effFormat = doc.Fields["format"]
		}

		if jsonOutput {
			out := map[string]any{
				"id":        docID,
				"format":    effFormat,
				"unchanged": !changed,
			}
			if bundleID != "" {
				out["bundle"] = bundleID
				out["url"] = bundleURL(bundleID) + "#doc-" + docID
			}
			printJSON(out)
			return nil
		}

		if changed {
			fmt.Printf("Doc %s updated in place (format=%s)\n", docID, effFormat)
		} else {
			fmt.Printf("Doc %s already matches — nothing written\n", docID)
		}
		if bundleID != "" {
			fmt.Printf("  %s#doc-%s\n", bundleURL(bundleID), docID)
		}
		fmt.Printf("  Read it back: gb bundle doc %s\n", docID)
		return nil
	},
}

// ── gb bundle order ─────────────────────────────────────────────────────

var bundleOrderCmd = &cobra.Command{
	Use:   "order <bundle-id> <doc-id> <position>",
	Short: "Set the table-of-contents position of a doc",
	Long: `Set the integer position of a doc within a bundle's TOC.

The viewer sorts docs by fields.position ascending. Lower numbers render
first; ties fall back to creation order.`,
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		bundleID := args[0]
		docID := args[1]
		position, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("position must be an integer: %w", err)
		}

		// Verify the bundle is actually a bundle.
		bundle, err := daemon.GetBead(cmd.Context(), bundleID)
		if err != nil {
			return fmt.Errorf("fetching bundle %s: %w", bundleID, err)
		}
		if bundle.Type != "bundle" {
			return fmt.Errorf("%s is not a bundle (type=%s)", bundleID, bundle.Type)
		}

		// Verify the doc bead exists (no parent-child enforcement here — it's
		// cheap to set position even for unlinked beads, and add can re-link).
		if _, err := daemon.GetBead(cmd.Context(), docID); err != nil {
			return fmt.Errorf("fetching doc %s: %w", docID, err)
		}

		// position is an integer-typed schema field; the daemon validator
		// rejects a string value with HTTP 400 "position: must be an integer".
		// Send it typed so the value serializes as a JSON number (ka-0000005409).
		if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), docID, map[string]any{
			"position": position,
		}); err != nil {
			return fmt.Errorf("setting position: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"bundle":   bundleID,
				"doc":      docID,
				"position": position,
			})
		} else {
			fmt.Printf("Set %s position=%d in bundle %s\n", docID, position, bundleID)
		}
		return nil
	},
}

// ── gb bundle remove ────────────────────────────────────────────────────

var bundleRemoveCmd = &cobra.Command{
	Use:     "remove <bundle-id> <doc-id>",
	Aliases: []string{"detach", "rm"},
	Short:   "Detach a doc from a bundle so it no longer renders in the TOC",
	Long: `Remove a doc from a bundle's reading surface.

Deletes the parent-child edge that links the doc under the bundle, so the doc
stops rendering in 'gb bundle show' and the /bundle/<id> viewer TOC. This is the
supported way to restore a single plan-of-record after a doc is superseded,
double-added, or left over from an interrupted cleanup.

Detach-only by default (safer): the doc bead itself is NOT closed or deleted, so
any other reference to it (a link elsewhere, a second bundle) survives. Closing
a doc bead does NOT detach it — you must run this command. Pass --close to also
close the doc bead in the same step when it is genuinely obsolete.

Example:
  gb bundle remove ka-Bundle123 ka-Doc456           # detach only
  gb bundle detach ka-Bundle123 ka-Doc456 --close   # detach + close the doc`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		bundleID := args[0]
		docID := args[1]
		alsoClose, _ := cmd.Flags().GetBool("close")

		// Verify the bundle is actually a bundle so a typo'd id fails clearly
		// rather than silently no-op'ing the edge delete.
		bundle, err := daemon.GetBead(cmd.Context(), bundleID)
		if err != nil {
			return fmt.Errorf("fetching bundle %s: %w", bundleID, err)
		}
		if bundle.Type != "bundle" {
			return fmt.Errorf("%s is not a bundle (type=%s)", bundleID, bundle.Type)
		}

		// Verify the doc is currently linked under the bundle, so removing an
		// already-detached (or never-linked) doc reports a clear error instead
		// of a silent success. Reads the same reverse parent-child edges the
		// viewer TOC renders from.
		revDeps, err := daemon.GetReverseDependencies(cmd.Context(), bundleID)
		if err != nil {
			return fmt.Errorf("listing bundle children: %w", err)
		}
		linked := false
		for _, d := range revDeps {
			if d.BeadID == docID && d.Type == "parent-child" {
				linked = true
				break
			}
		}
		if !linked {
			return fmt.Errorf("%s is not a member of bundle %s (nothing to detach)", docID, bundleID)
		}

		// Detach: delete the doc → bundle parent-child edge. This is what drops
		// the doc from `gb bundle show` and the viewer TOC.
		if err := daemon.RemoveDependency(cmd.Context(), docID, bundleID, "parent-child"); err != nil {
			return fmt.Errorf("detaching %s from bundle %s: %w", docID, bundleID, err)
		}

		// Optionally close the doc bead. Detach-only is the default (the bead
		// may be referenced elsewhere); --close is opt-in for genuinely obsolete
		// docs.
		if alsoClose {
			if err := daemon.CloseBead(cmd.Context(), docID, nil); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: detached but failed to close doc bead: %v\n", err)
			}
		}

		if jsonOutput {
			printJSON(map[string]any{
				"bundle":   bundleID,
				"doc":      docID,
				"detached": true,
				"closed":   alsoClose,
				"url":      bundleURL(bundleID),
			})
		} else {
			if alsoClose {
				fmt.Printf("Detached %s from bundle %s and closed the doc bead\n", docID, bundleID)
			} else {
				fmt.Printf("Detached %s from bundle %s (doc bead left open — pass --close to close it)\n", docID, bundleID)
			}
			fmt.Printf("  %s\n", bundleURL(bundleID))
		}
		return nil
	},
}

// ── gb bundle locate ────────────────────────────────────────────────────

var bundleLocateCmd = &cobra.Command{
	Use:     "locate <origin-bead-id>",
	Aliases: []string{"find", "of"},
	Short:   "Find the bundle(s) attached to an origin/plan bead",
	Long: `Answer "does this plan/origin bead have a bundle, and where is it?".

Every other 'gb bundle' subcommand requires you to ALREADY KNOW the bundle id.
This is the reverse lookup: given the origin (a plan epic, a task, any bead you
attached deliverables to), print the bundle id and its /bundle/<id> viewer URL
in one call. Reads the same parent-child edge 'gb bundle create' writes — the
bundle is the child of its origin — so it covers every bundle uniformly.

An origin with NO bundle is an authoritative negative, not an error: it prints
"no bundle attached" and exits 0, so a gardener can branch on it cleanly.

Example:
  gb bundle locate ka-Plan123          # -> bundle id + viewer URL
  gb bundle locate ka-Plan123 --json   # {"origin","bundles":[{"id","url",...}]}
  gb bundle show "$(gb bundle locate ka-Plan123 --json | jq -r '.bundles[0].id')"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		originID := args[0]

		// Verify the origin exists so a typo'd id fails loudly instead of
		// masquerading as an authoritative "no bundle attached".
		origin, err := daemon.GetBead(cmd.Context(), originID)
		if err != nil {
			return fmt.Errorf("fetching origin %s: %w", originID, err)
		}

		// A bundle is created as a parent-child CHILD of its origin (see
		// bundleCreateCmd), so the bundles attached to this bead are exactly
		// its reverse parent-child deps whose type is `bundle`.
		revDeps, err := daemon.GetReverseDependencies(cmd.Context(), originID)
		if err != nil {
			return fmt.Errorf("listing children of %s: %w", originID, err)
		}

		type bundleEntry struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			Title    string `json:"title"`
			Kind     string `json:"kind"`
			Headline string `json:"headline,omitempty"`
		}

		seen := map[string]bool{}
		bundles := []bundleEntry{}
		for _, d := range revDeps {
			if d.Type != "parent-child" || seen[d.BeadID] {
				continue
			}
			seen[d.BeadID] = true
			child, err := daemon.GetBead(cmd.Context(), d.BeadID)
			if err != nil {
				continue
			}
			if child.Type != "bundle" {
				continue
			}
			bundles = append(bundles, bundleEntry{
				ID:       child.ID,
				URL:      bundleURL(child.ID),
				Title:    child.Title,
				Kind:     child.Fields["kind"],
				Headline: child.Fields["headline"],
			})
		}

		// Stable output: reverse-dep order is not guaranteed by the daemon.
		sort.SliceStable(bundles, func(i, j int) bool { return bundles[i].ID < bundles[j].ID })

		if jsonOutput {
			printJSON(map[string]any{
				"origin":  originID,
				"bundles": bundles,
			})
			return nil
		}

		if len(bundles) == 0 {
			// Authoritative negative — stdout + exit 0, so callers can branch.
			fmt.Printf("no bundle attached to %s\n", originID)
			if origin.Type == "bundle" {
				fmt.Printf("  (%s is itself a bundle — did you mean `gb bundle show %s`?)\n", originID, originID)
			} else {
				fmt.Printf("  (attach one: gb bundle create %s --title <title> --kind plan)\n", originID)
			}
			return nil
		}

		fmt.Printf("Bundle(s) attached to %s (%s):\n", originID, withArms(origin.Heraldry, origin.Title))
		for _, b := range bundles {
			label := b.Headline
			if label == "" {
				label = b.Title
			}
			fmt.Printf("  %s [%s] %s\n", b.ID, b.Kind, label)
			fmt.Printf("    %s\n", b.URL)
		}
		fmt.Println("  (read the TOC: gb bundle show <bundle-id>)")
		return nil
	},
}

// ── gb bundle show ──────────────────────────────────────────────────────

var bundleShowCmd = &cobra.Command{
	Use:   "show <bundle-id>",
	Short: "Print a bundle's table of contents and viewer URL",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bundleID := args[0]
		bundle, err := daemon.GetBead(cmd.Context(), bundleID)
		if err != nil {
			return fmt.Errorf("fetching bundle %s: %w", bundleID, err)
		}
		if bundle.Type != "bundle" {
			return fmt.Errorf("%s is not a bundle (type=%s)", bundleID, bundle.Type)
		}

		// Walk reverse graph-dependency edges to find docs parented to this
		// bundle. Reads the same `deps` table that `gb bundle add` writes to
		// — ka-0000003477: mentioned-by reads `bead_references` which is
		// only populated when the doc text contains a typed mention. Reverse
		// deps cover both new (with typed mention) and legacy (graph-only)
		// bundles uniformly.
		revDeps, err := daemon.GetReverseDependencies(cmd.Context(), bundleID)
		if err != nil {
			return fmt.Errorf("listing bundle children: %w", err)
		}

		type docEntry struct {
			ID       string
			Title    string
			Format   string
			Headline string
			Position int
		}

		seen := map[string]bool{}
		var docs []docEntry
		for _, d := range revDeps {
			if seen[d.BeadID] {
				continue
			}
			if d.Type != "parent-child" {
				continue
			}
			seen[d.BeadID] = true
			child, err := daemon.GetBead(cmd.Context(), d.BeadID)
			if err != nil {
				continue
			}
			if child.Type != "doc" {
				continue
			}
			pos := 0
			if p, ok := child.Fields["position"]; ok && p != "" {
				if n, err := strconv.Atoi(p); err == nil {
					pos = n
				}
			}
			docs = append(docs, docEntry{
				ID:       child.ID,
				Title:    child.Title,
				Format:   child.Fields["format"],
				Headline: child.Fields["headline"],
				Position: pos,
			})
		}

		sort.SliceStable(docs, func(i, j int) bool {
			if docs[i].Position != docs[j].Position {
				return docs[i].Position < docs[j].Position
			}
			return docs[i].ID < docs[j].ID
		})

		viewerURL := bundleURL(bundleID)

		if jsonOutput {
			out := map[string]any{
				"id":    bundle.ID,
				"title": bundle.Title,
				"kind":  bundle.Fields["kind"],
				"url":   viewerURL,
				"docs":  docs,
			}
			printJSON(out)
			return nil
		}

		fmt.Printf("Bundle %s: %s\n", bundle.ID, withArms(bundle.Heraldry, bundle.Title))
		if k := bundle.Fields["kind"]; k != "" {
			fmt.Printf("  Kind: %s\n", k)
		}
		if h := bundle.Fields["headline"]; h != "" {
			fmt.Printf("  Headline: %s\n", h)
		}
		fmt.Printf("  URL: %s\n", viewerURL)
		if len(docs) > 0 {
			// The read-hint sits ABOVE the doc list (and repeats on every doc line
			// below) so `gb bundle show <id> | head -5` — the common way to peek at
			// a TOC — cannot truncate it away. It used to be the LAST line printed,
			// which put it out of reach of any `head`, so readers improvised
			// malformed commands instead (ka-0000002294).
			fmt.Printf("  Read a doc body: gb bundle doc <doc-id>  (e.g. gb bundle doc %s)\n", docs[0].ID)
		}
		fmt.Printf("  Docs (%d):\n", len(docs))
		if len(docs) == 0 {
			fmt.Println("    (none yet — use `gb bundle add` to attach docs)")
			return nil
		}
		for i, d := range docs {
			label := d.Headline
			if label == "" {
				label = d.Title
			}
			fmt.Printf("    %d. [%s] %s — %s (pos=%d)  ·  read: gb bundle doc %s\n",
				i+1, d.Format, d.ID, label, d.Position, d.ID)
		}
		return nil
	},
}

// ── gb bundle doc ───────────────────────────────────────────────────────

var bundleDocCmd = &cobra.Command{
	Use:   "doc <doc-id>",
	Short: "Print the rendered body of a type=doc bead",
	Long: `Print the rendered content of a 'type=doc' bead to stdout.

A doc id is globally unique, so only the doc id is needed — you do not have to
name the parent bundle. This is the first-class way to read a doc body listed
by 'gb bundle show'; the raw fallback is 'kd show <id> --json | jq -r .fields.content'.

Inline docs print fields.content. Out-of-band docs (uploaded via --content-url)
have no inline body, so their fields.content_url is printed instead.

Example:
  gb bundle doc ka-Doc123          # print the markdown body
  gb bundle doc ka-Doc123 --json   # {"id","title","format","headline","content","content_url"}`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		docID := args[0]
		doc, err := daemon.GetBead(cmd.Context(), docID)
		if err != nil {
			return fmt.Errorf("fetching doc %s: %w", docID, err)
		}
		if doc.Type != "doc" {
			return fmt.Errorf("%s is not a doc (type=%s)", docID, doc.Type)
		}

		content := doc.Fields["content"]
		contentURL := doc.Fields["content_url"]

		if jsonOutput {
			printJSON(map[string]string{
				"id":          doc.ID,
				"title":       doc.Title,
				"format":      doc.Fields["format"],
				"headline":    doc.Fields["headline"],
				"content":     content,
				"content_url": contentURL,
			})
			return nil
		}

		// Print the inline body raw so it round-trips cleanly through a pipe.
		// Out-of-band docs carry no inline content — surface the URL instead.
		if content != "" {
			fmt.Print(content)
			if !strings.HasSuffix(content, "\n") {
				fmt.Println()
			}
			return nil
		}
		if contentURL != "" {
			fmt.Printf("(no inline body — content hosted out-of-band)\n%s\n", contentURL)
			return nil
		}
		fmt.Fprintln(os.Stderr, "(doc has no content or content_url)")
		return nil
	},
}

// ── helpers ─────────────────────────────────────────────────────────────

// bundleURL returns the canonical /bundle/<id> URL for a bundle. When
// BEADS_VIEWER_URL is set, it is prepended; otherwise we fall back to the
// built-in defaultGardenioViewerURL (which carries the /beads path prefix) so
// the printed link is always absolute and clickable — matching imageProxyURL.
//
// Agent pods do NOT get BEADS_VIEWER_URL injected, so before ka-0000003798 an
// agent's `gb bundle` link (e.g. a squawked design-panel bundle) was emitted
// as a hostless, prefix-less "/bundle/<id>". Slack rendered that as plain
// non-clickable text, and pasted onto the prod host it 404s (the viewer serves
// under /beads). Falling back to the absolute default makes every agent-posted
// bundle link land on the real page.
func bundleURL(id string) string {
	base := strings.TrimRight(os.Getenv("BEADS_VIEWER_URL"), "/")
	if base == "" {
		base = strings.TrimRight(defaultGardenioViewerURL, "/")
	}
	return base + "/bundle/" + url.PathEscape(id)
}

// formatFromExt maps a file path to a doc format. Unknown extensions return
// the empty string so the caller can require an explicit --format.
func formatFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "md"
	case ".diff", ".patch":
		return "diff"
	case ".csv":
		return "csv"
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp":
		return "image"
	case ".html", ".htm":
		return "html"
	case ".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx", ".sh", ".yaml", ".yml", ".json", ".toml", ".css":
		return "code"
	}
	return ""
}

func init() {
	bundleCmd.AddCommand(bundleCreateCmd)
	bundleCmd.AddCommand(bundleAddCmd)
	bundleCmd.AddCommand(bundleUpdateCmd)
	bundleCmd.AddCommand(bundleRemoveCmd)
	bundleCmd.AddCommand(bundleOrderCmd)
	bundleCmd.AddCommand(bundleLocateCmd)
	bundleCmd.AddCommand(bundleShowCmd)
	bundleCmd.AddCommand(bundleDocCmd)

	bundleCreateCmd.Flags().String("title", "", "bundle title (required)")
	bundleCreateCmd.Flags().String("kind", "report", "bundle kind: plan, post-mortem, spec, report, other")
	bundleCreateCmd.Flags().String("headline", "", "optional one-line headline")
	bundleCreateCmd.Flags().String("summary", "", "optional summary paragraph")

	bundleAddCmd.Flags().String("doc", "", "create a doc bead from this file path")
	bundleAddCmd.Flags().String("content-url", "", "create a doc bead pointing at this URL (for >256KB or binary content)")
	bundleAddCmd.Flags().String("format", "", "doc format: md, diff, csv, code, image, link, html (inferred from --doc extension when possible; html renders in a sandboxed iframe)")
	bundleAddCmd.Flags().String("title", "", "override the doc bead title")
	bundleAddCmd.Flags().String("headline", "", "optional one-line headline shown in the TOC")
	bundleAddCmd.Flags().String("language", "", "language hint for `code` format (e.g. go, rust)")
	bundleAddCmd.Flags().Int("position", 0, "position in the bundle TOC (lower renders first)")

	bundleUpdateCmd.Flags().String("doc", "", "replace the doc body with the contents of this file path")
	bundleUpdateCmd.Flags().String("content-url", "", "switch the doc to this out-of-band URL (for >256KB or binary content)")
	bundleUpdateCmd.Flags().String("format", "", "doc format: md, diff, csv, code, image, link, html (default: keep the doc's current format)")
	bundleUpdateCmd.Flags().String("title", "", "also rewrite the doc bead title")
	bundleUpdateCmd.Flags().String("headline", "", "also rewrite the one-line headline shown in the TOC (pass empty to clear it)")
	bundleUpdateCmd.Flags().String("language", "", "language hint for `code` format (e.g. go, rust)")

	bundleRemoveCmd.Flags().Bool("close", false, "also close the doc bead (default: detach only — the bead may be referenced elsewhere)")
}
