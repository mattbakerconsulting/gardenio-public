package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commentCmd is the `kd comment` group. Like `kd dep` (see dep.go and group.go),
// it carries its OWN inline RunE + Args instead of going through the shared
// markAsGroup() helper, because it needs a smarter default than "bare → help":
//
//   - `kd comment <id> <text...>` (first token is a bead id) is the
//     most-common-subcommand affordance — it Does The Right Thing by delegating
//     to the `add` path (the git/gh/glab "bare verb = top subcommand" pattern).
//     Previously this errored `unknown command "ka-XXXX" for "kd comment"` and
//     posted ZERO comments, silently dropping the text (ka-0000006021) — even
//     though the data plane's own docs steered agents into this exact form.
//   - `kd comment` (no args) still falls through to Help() (exit 0).
//   - `kd comment <non-id>` (e.g. `kd comment bogus`) is still rejected as an
//     unknown subcommand, preserving the no-silent-exit-0 group contract
//     enforced by group_noargs_test.go. commentCmd is therefore intentionally
//     EXCLUDED from group.go's markAsGroup registry (which would overwrite the
//     RunE/Args below with NoArgs+Help).
//
// KNOWN subcommands (add/list/delete/trim) match first in cobra's Find and run
// their own RunE/Args, never reaching this default RunE.
var commentCmd = &cobra.Command{
	Use:     "comment",
	Short:   "Manage bead comments",
	GroupID: "beads",
	Args:    commentArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			// A truly-empty `kd comment` shows group help (exit 0). But
			// `kd comment --body '…'` with NO bead-id positional is a missing
			// argument, not a help request — erroring keeps the bare form from
			// silently dropping text the same way the original bug did (the
			// agent who forgot the id would otherwise see help and assume the
			// note posted).
			if cmd.Flags().Changed("body") {
				return fmt.Errorf("a bead id is required: kd comment <bead-id> --body '…' (or kd comment <bead-id> <text…>)")
			}
			return cmd.Help()
		}
		// args[0] is a bead id (validated by commentArgs) → treat the bare
		// `kd comment <id> <text...>` as `kd comment add <id> <text...>`.
		return runCommentAdd(cmd, args)
	},
}

// looksLikeBeadID reports whether tok is shaped like a bead id (gd-…). The bare
// `kd comment <tok> …` default form only delegates to the add path when tok is a
// bead id; any other first token is treated as an unknown subcommand so real
// typos (`kd comment frobnicate`) still surface an error rather than being
// mistaken for a comment target.
func looksLikeBeadID(tok string) bool {
	return (strings.HasPrefix(tok, "gd-") || strings.HasPrefix(tok, "ka-"))
}

// commentArgs validates the bare `kd comment …` form. No args is fine (falls
// through to Help). A bead-id first token is fine (delegates to add). Any other
// first token is an unknown subcommand and is rejected with cobra's standard
// `unknown command %q for %q` message — the same rejection markAsGroup()/
// cobra.NoArgs produce, so the no-silent-exit-0 contract (group_noargs_test.go)
// still holds for `kd comment bogus`.
func commentArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if !looksLikeBeadID(args[0]) {
		return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return nil
}

// runCommentAdd is the shared add body invoked by BOTH `kd comment add` and the
// bare `kd comment <id> <text...>` default form, so the two paths can never
// diverge. args[0] is the bead id; args[1:] is the positional text (mutually
// exclusive with --body/-m via resolveCommentText).
func runCommentAdd(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	text, err := resolveCommentText(cmd.Flags().Changed("body"), commentBody, args[1:])
	if err != nil {
		return err
	}

	c, err := beadsClient.AddComment(context.Background(), beadID, actor, text)
	if err != nil {
		return fmt.Errorf("adding comment: %w", err)
	}

	if jsonOutput {
		emitJSON(c)
	} else {
		// Terse single-line success by default (ka-0000005647), matching
		// claim/close/done: the multi-field dump trained agents to mask output.
		// Use --json for the full comment record (id, author, text, timestamp).
		fmt.Printf("Comment %d added to %s\n", c.ID, c.BeadID)
	}
	return nil
}

// commentBody holds the value of the --body/-m flag on `kd comment add`. It is
// an alternative to supplying the comment text positionally, matching the
// git/gh/glab convention and the `kd comment add <id> --body '…'` guidance that
// gb already prints (see gardenio .../gb/decision.go and operator_refusal.go).
var commentBody string

// resolveCommentText derives the comment text from exactly one source: the
// --body/-m flag (bodySet reports whether the flag was supplied, even with an
// empty value) or the positional text args (everything after <bead-id>). It
// enforces the flag-XOR-positional contract and rejects empty text. The
// empty-text guard lives here rather than in cobra's Args because relaxing the
// command to MinimumNArgs(1) — required so `kd comment add <id> --body x`
// parses with a single positional — no longer rejects a text-less invocation at
// the cobra layer.
//
// The flag value is expanded through resolveTextValue, so `--body @file` reads
// the file (and `--body @-` reads stdin) exactly as `kd create -d @file` does
// (ka-0000005879). Before this, posting a markdown file meant routing it
// through the shell, and every such route mangles the body silently at exit 0
// (standing advice ka-0000004424, ka-0000002095):
//
//   - `--body "$(cat f.md)"` strips ALL trailing newlines (command
//     substitution's documented behavior), so fenced code blocks and trailing
//     list items lose their terminator.
//   - the same unquoted word-splits and glob-expands the body into argv.
//   - an UNQUOTED heredoc (`<<EOF`) additionally EXECUTES backtick and $(…)
//     spans in the file — markdown inline code becomes command output. Only a
//     quoted heredoc (`<<'EOF'`) is safe, which is the non-obvious part agents
//     kept getting wrong.
//
// `@file` sidesteps all of it: the bytes never touch a shell. Positional text
// is left verbatim — a bare positional cannot unambiguously carry @file intent
// (`kd comment add <id> @home tomorrow` is prose) — and `--body @@…` remains
// the escape for a literal leading '@'.
func resolveCommentText(bodySet bool, body string, textArgs []string) (string, error) {
	hasPositional := len(textArgs) > 0
	if bodySet && hasPositional {
		return "", fmt.Errorf("provide comment text via positional <text> OR --body/-m, not both")
	}
	text := strings.Join(textArgs, " ")
	if bodySet {
		expanded, err := resolveTextValue(body)
		if err != nil {
			return "", err
		}
		text = expanded
	}
	if text == "" {
		return "", fmt.Errorf("comment text required (positional <text> or --body/-m)")
	}
	return text, nil
}

var commentAddCmd = &cobra.Command{
	Use:   "add <bead-id> [<text>...]",
	Short: "Add a comment to a bead",
	Long: `Add a comment to a bead.

The comment text may be supplied positionally (kd comment add <id> some text)
or via the --body/-m flag (kd comment add <id> --body 'some text'). --content
and --message are accepted as aliases for --body. Exactly one source must be
used. Use --body, or put -- before the text, when the comment begins with a
dash (kd comment add <id> --body '-fixed in 2026.x').

--body expands curl-style @file: --body @notes.md reads the comment text from
notes.md and --body @- reads it from stdin, both verbatim, bytes unchanged.
Prefer that to routing a file through the shell, which mangles it silently:
--body "$(cat notes.md)" strips every trailing newline, the unquoted form also
word-splits it, and an unquoted heredoc (<<EOF) executes backtick and $(…)
spans inside the file. Use @@ to pass a literal value beginning with '@'
(kd comment add <id> --body '@@mention …').`,
	Args: cobra.MinimumNArgs(1),
	RunE: runCommentAdd,
}

// commentSince holds the --since/--after value on `kd comment list`: either an
// RFC3339 timestamp or a duration back from now ("4h", "30m", "2d").
var commentSince string

// parseCommentSince resolves a --since value into an absolute cutoff. It
// accepts an RFC3339 timestamp or a duration (subtracted from now; 'd' days
// supported via parseDuration). Watchdogs that wake on an interval pass the
// interval as a duration instead of hand-rolling timestamp greps against the
// pretty-printed output (ka-0000004918: one such grep silently matched nothing
// for minutes 06-59, hiding operator comments). The parsing lives in
// parseTimeCutoff, shared with the `kd list` time-window flags.
func parseCommentSince(val string, now time.Time) (time.Time, error) {
	return parseTimeCutoff("--since", val, now)
}

// filterCommentsSince keeps comments created at-or-after cutoff. The boundary
// is inclusive so a caller replaying "since my last observed comment's
// timestamp" cannot drop that exact comment.
func filterCommentsSince(comments []*model.Comment, cutoff time.Time) []*model.Comment {
	out := make([]*model.Comment, 0, len(comments))
	for _, c := range comments {
		if !c.CreatedAt.Before(cutoff) {
			out = append(out, c)
		}
	}
	return out
}

// aliasCommentSinceFlags normalizes `--after` → `--since` on `kd comment list`,
// mirroring aliasCommentBodyFlags: both spellings are natural and should
// resolve to the same flag instead of an unknown-flag usage dump.
func aliasCommentSinceFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "after" {
			name = "since"
		}
		return pflag.NormalizedName(name)
	})
}

var commentListCmd = &cobra.Command{
	Use:   "list <bead-id>",
	Short: "List comments on a bead",
	Long: `List comments on a bead, oldest first.

Use --since (alias --after) to show only comments created at-or-after a cutoff,
given as an RFC3339 timestamp or a duration back from now:

  kd comment list ka-abc123 --since 4h
  kd comment list ka-abc123 --since 2026-07-18T09:00:00Z`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]

		comments, err := beadsClient.GetComments(context.Background(), beadID)
		if err != nil {
			return fmt.Errorf("listing comments: %w", err)
		}
		if cmd.Flags().Changed("since") {
			cutoff, err := parseCommentSince(commentSince, time.Now())
			if err != nil {
				return err
			}
			comments = filterCommentsSince(comments, cutoff)
		}
		if comments == nil {
			// Keep --json emitting [] rather than null for the empty case.
			comments = []*model.Comment{}
		}

		if jsonOutput {
			emitJSON(comments)
		} else {
			if len(comments) == 0 {
				fmt.Println("No comments found.")
				return nil
			}
			for i, c := range comments {
				if i > 0 {
					fmt.Println("---")
				}
				createdAt := ""
				if !c.CreatedAt.IsZero() {
					createdAt = c.CreatedAt.Format("2006-01-02 15:04:05")
				}
				fmt.Printf("[%s] %s:\n  %s\n", createdAt, c.Author, c.Text)
			}
		}
		return nil
	},
}

var commentDeleteCmd = &cobra.Command{
	Use:   "delete <bead-id> <comment-id>",
	Short: "Delete a single comment by id",
	Long: `Delete a single comment by id.

Idempotent: a missing comment yields a successful response with deleted=false
so retries are safe. Every successful delete writes an audit event row.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		commentID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || commentID <= 0 {
			return fmt.Errorf("comment-id must be a positive integer: %q", args[1])
		}
		deleted, err := beadsClient.DeleteComment(context.Background(), beadID, commentID)
		if err != nil {
			return fmt.Errorf("deleting comment: %w", err)
		}
		if jsonOutput {
			data, _ := json.MarshalIndent(map[string]any{"deleted": deleted, "comment_id": commentID}, "", "  ")
			fmt.Println(string(data))
			return nil
		}
		if deleted {
			fmt.Printf("Deleted comment %d on %s\n", commentID, beadID)
		} else {
			fmt.Printf("Comment %d on %s already gone\n", commentID, beadID)
		}
		return nil
	},
}

var (
	trimKeepLast int
	trimAuthor   string
)

var commentTrimCmd = &cobra.Command{
	Use:   "trim <bead-id>",
	Short: "Trim a bead's comments to the most recent N",
	Long: `Trim a bead's comments, keeping only the most recent --keep-last entries.

When --author is set, both the keep window and the deletion are scoped to
that author — comments by other authors are never touched. This is the
safe shape for the scheduler to reap its own noisy comments without
disturbing human/agent discussion.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		if trimKeepLast < 0 {
			return fmt.Errorf("--keep-last must be >= 0")
		}
		ids, err := beadsClient.TrimComments(context.Background(), beadID, trimKeepLast, trimAuthor)
		if err != nil {
			return fmt.Errorf("trimming comments: %w", err)
		}
		if jsonOutput {
			data, _ := json.MarshalIndent(map[string]any{"deleted_ids": ids, "deleted_count": len(ids)}, "", "  ")
			fmt.Println(string(data))
			return nil
		}
		if len(ids) == 0 {
			fmt.Printf("No comments to trim on %s\n", beadID)
			return nil
		}
		fmt.Printf("Trimmed %d comments on %s\n", len(ids), beadID)
		return nil
	},
}

// aliasCommentBodyFlags normalizes `--content` and `--message` → `--body` on the
// comment-add paths so the spellings agents naturally reach for resolve to the
// same flag instead of a bare cobra "unknown flag: --content" usage dump that
// silently drops the comment text (ka-0000001669: a gardener session lost a round
// trip on `--content`, and the `2>&1 | head -5` tail masked the non-zero exit).
// The vocabulary is inconsistent across the suite — `gb decision report` takes
// --content while `kd close` takes --reason — so agents transfer flags between
// commands. Mirrors aliasCloseReasonFlags (close.go) and aliasLabelFlags
// (label_alias.go); attached PER COMMAND, never global, so no other command's
// flags are shadowed. -m stays the shorthand for --body only.
func aliasCommentBodyFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "content", "message":
			name = "body"
		}
		return pflag.NormalizedName(name)
	})
}

func init() {
	commentAddCmd.Flags().StringVarP(&commentBody, "body", "m", "", "comment text (alternative to positional <text>); @file reads the value from a file, @- reads stdin, @@ escapes a literal leading '@'; aliases: --content, --message")
	aliasCommentBodyFlags(commentAddCmd)
	// The bare `kd comment <id> --body '…'` default form executes commentCmd's
	// own RunE, so commentCmd needs its own --body/-m local flag (bound to the
	// same commentBody var) for the flag to parse and the XOR contract to fire.
	// The --content/--message aliases are normalized on this path too, since the
	// bare `kd comment <id> --content '…'` form parses with commentCmd's flags.
	commentCmd.Flags().StringVarP(&commentBody, "body", "m", "", "comment text (alternative to positional <text>); @file reads the value from a file, @- reads stdin, @@ escapes a literal leading '@'; aliases: --content, --message")
	aliasCommentBodyFlags(commentCmd)
	commentListCmd.Flags().StringVar(&commentSince, "since", "", "only comments created at-or-after this RFC3339 timestamp or duration back from now (4h, 30m, 2d); alias: --after")
	aliasCommentSinceFlags(commentListCmd)
	commentTrimCmd.Flags().IntVar(&trimKeepLast, "keep-last", 30, "keep the most recent N comments")
	commentTrimCmd.Flags().StringVar(&trimAuthor, "author", "", "scope trim to comments by this author (empty = all authors)")

	commentCmd.AddCommand(commentAddCmd)
	commentCmd.AddCommand(commentListCmd)
	commentCmd.AddCommand(commentDeleteCmd)
	commentCmd.AddCommand(commentTrimCmd)
}
