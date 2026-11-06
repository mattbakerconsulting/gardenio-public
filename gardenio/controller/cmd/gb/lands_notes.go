package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/steward"

	"github.com/spf13/cobra"
)

// gb lands notes is the steward's memory, the land's steward-notes:<land> doc
// bead (amendment 2026-10-23 §3, step 9.2a): read it, or write it back at the
// end of a run. The write keeps the stored Holds (the section people edit),
// applies only explicit adds and lifts to it, and holds the notes to the
// 4 KB cap by rolling the oldest Answers into a comment on the bead.

var landsNotesCmd = &cobra.Command{
	Use:   "notes <land>",
	Short: "Read or write a land's steward notes (holds, answers, promises, in flight)",
	Long: `With no flags, prints the land's steward-notes:<land> bead.

--write <file> (or - for stdin) replaces the Answers, Promises and In flight
sections with the file's; its Holds section, if any, is ignored and the stored
Holds are kept, so a person's edit made during the run is not lost. Change
Holds with --add-hold and --lift-hold (each repeatable; usable without
--write). A lift names one hold by its text, or a part of it no other hold
shares.

The notes are held to 4096 bytes: the oldest Answers roll out, newest first,
into a comment on the bead. Notes still too long are refused; shorten In
flight or Promises.

--machines-high-water <RFC 3339> records the newest #machines message read,
where the next run's delta starts.`,
	Example: `  gb lands notes tomato-patch
  gb lands notes tomato-patch --write notes.md --machines-high-water 2026-10-23T05:12:09.5Z
  gb lands notes tomato-patch --add-hold "PR 12: ada, 10-23: not until she has read it"
  gb lands notes tomato-patch --lift-hold "PR 12"`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsNotes,
}

func runLandsNotes(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	land := args[0]
	file, _ := cmd.Flags().GetString("write")
	add, _ := cmd.Flags().GetStringArray("add-hold")
	lift, _ := cmd.Flags().GetStringArray("lift-hold")
	mark, _ := cmd.Flags().GetString("machines-high-water")
	if mark != "" {
		if _, err := time.Parse(time.RFC3339, mark); err != nil {
			return fmt.Errorf("--machines-high-water %q is not RFC 3339", mark)
		}
	}

	if file == "" && len(add) == 0 && len(lift) == 0 && mark == "" {
		b, err := steward.FindNotes(ctx, daemon, land)
		if err != nil {
			return err
		}
		if b == nil {
			return fmt.Errorf("land %s has no %s bead", land, steward.NotesTitle(land))
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{
				"id": b.ID, "content": steward.NotesBody(b), steward.NotesHighWaterField: b.Fields[steward.NotesHighWaterField],
			})
		}
		fmt.Printf("%s (%s)\n\n%s\n", steward.NotesTitle(land), b.ID, steward.NotesBody(b))
		return nil
	}

	edit := steward.NotesEdit{AddHolds: add, LiftHolds: lift}
	if file != "" {
		var raw []byte
		var err error
		if file == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(file)
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", file, err)
		}
		if strings.TrimSpace(string(raw)) == "" {
			return fmt.Errorf("%s is empty; the notes are not cleared by an empty write", file)
		}
		edit.Written = string(raw)
	}
	res, err := steward.WriteNotes(ctx, daemon, daemon, land, edit, mark, landsOperator())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Printf("wrote %s (%s), %d bytes", steward.NotesTitle(land), res.ID, res.Bytes)
	if n := len(res.Rolled); n > 0 {
		fmt.Printf("; %d oldest answer(s) rolled into a comment", n)
	}
	fmt.Println()
	return nil
}

func init() {
	landsNotesCmd.Flags().String("write", "", "file with the Answers, Promises and In flight sections (- for stdin)")
	landsNotesCmd.Flags().StringArray("add-hold", nil, "add a hold (repeatable)")
	landsNotesCmd.Flags().StringArray("lift-hold", nil, "remove the one hold this text names (repeatable)")
	landsNotesCmd.Flags().String("machines-high-water", "", "newest #machines message read (RFC 3339)")
	landsCmd.AddCommand(landsNotesCmd)
}
