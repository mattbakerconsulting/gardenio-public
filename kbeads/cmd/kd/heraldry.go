package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rivo/uniseg"
	"github.com/spf13/cobra"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/heraldry"
	"gardenio/kbeads/internal/model"
)

// heraldry.go is kd's side of a bead's arms (heraldry spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §3.2, §4.2,
// §7; steps 1.3 gd-FcIEiHsjJD and 5.3 gd-InkEAAqGNM): the --heraldry check
// before a create or update is sent, the Arms: line in `kd show`, the ARMS
// column in `kd list` and `kd search`, and `kd heraldry library` and `kd
// heraldry check`. Arms are a run of emoji, and the run is all a person sees.

// armsNone is the --heraldry value that removes arms (R7).
const armsNone = "none"

// checkArmsFlag validates a --heraldry value before it is sent, with the
// daemon's words (R11: a fast error, the same text). "" and "none" pass:
// they mean no arms.
func checkArmsFlag(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, armsNone) {
		return nil
	}
	if _, err := heraldry.Parse(raw); err != nil {
		return errors.New("invalid arms: " + err.Error() + ". " + model.HeraldryHelp)
	}
	return nil
}

// parseStoredArms reads a bead's stored arms, the run or a value still in the
// shield form; ok is false for none, or for a value this kd cannot read (a
// newer library on the daemon).
func parseStoredArms(stored string) (heraldry.Arms, bool) {
	if stored == "" {
		return heraldry.Arms{}, false
	}
	a, err := heraldry.Read(stored)
	if err != nil || a.Empty() {
		return heraldry.Arms{}, false
	}
	return a, true
}

// armsRun is the ARMS cell: the emoji run (§4.2), "" when the bead has none.
func armsRun(stored string) string {
	a, ok := parseStoredArms(stored)
	if !ok {
		return ""
	}
	return heraldry.Run(a)
}

// anyArms reports whether any bead bears arms, which is when a table grows
// its ARMS column.
func anyArms(beads []*model.Bead) bool {
	for _, b := range beads {
		if b != nil && armsRun(b.Heraldry) != "" {
			return true
		}
	}
	return false
}

// printArms prints kd show's Arms: line, the run. Nothing when the bead has
// no arms.
func printArms(out io.Writer, stored string) {
	if run := armsRun(stored); run != "" {
		fmt.Fprintf(out, "Arms:        %s\n", run)
	}
}

// wideCellMark stands in for a wide cell while text/tabwriter lays the table
// out. NUL cannot occur in any bead text (Postgres TEXT refuses it), so it
// never collides with a real cell.
const wideCellMark = 0

// wideCells lets text/tabwriter align a column whose cells are wider on
// screen than their rune count: tabwriter counts one column per rune, and an
// emoji is one rune drawn two columns wide (spec §3.2). Each wide cell goes
// into the table as a run of marks as long as its width on screen
// (uniseg.StringWidth); the tabwriter writes into wideCells, and Flush puts
// the cells back in place of their marks, in order, and writes the result to
// out. A table holds at most one wide column, so two runs of marks are always
// apart.
type wideCells struct {
	out   io.Writer
	buf   bytes.Buffer
	cells []string
}

func newWideCells(out io.Writer) *wideCells { return &wideCells{out: out} }

func (c *wideCells) Write(p []byte) (int, error) { return c.buf.Write(p) }

// cell returns the stand-in for s to put in the table. "" stays "".
func (c *wideCells) cell(s string) string {
	if s == "" {
		return ""
	}
	c.cells = append(c.cells, s)
	return strings.Repeat(string(rune(wideCellMark)), uniseg.StringWidth(s))
}

// Flush writes the laid-out table to out with every cell back in place. Call
// it after the tabwriter's Flush.
func (c *wideCells) Flush() error {
	b := c.buf.Bytes()
	var out bytes.Buffer
	for _, s := range c.cells {
		i := bytes.IndexByte(b, wideCellMark)
		if i < 0 {
			break
		}
		j := i
		for j < len(b) && b[j] == wideCellMark {
			j++
		}
		out.Write(b[:i])
		out.WriteString(s)
		b = b[j:]
	}
	out.Write(b)
	c.buf.Reset()
	c.cells = nil
	_, err := c.out.Write(out.Bytes())
	return err
}

var heraldryCmd = &cobra.Command{
	Use:     "heraldry",
	Short:   "Look up the emoji for a bead's arms and check arms before you give them",
	Long:    heraldryLong(),
	GroupID: "beads",
}

// heraldryLong is `kd heraldry --help`: how an agent gives an epic arms (spec
// R7, §7: agents pick arms, not humans), and then the one help text,
// model.HeraldryHelp. The same steps are the advice line's and
// gardenio/docs/heraldry/README.md's.
func heraldryLong() string {
	return "Arms are chosen, not generated: the agent that files or holds an epic (a plan or a phase) picks them. " +
		"Read the epic, choose three to five emoji that say what it is (first) and what it is for, and give them:\n\n" +
		"  1. Find emoji by name: kd heraldry library --search <word>\n" +
		"  2. See whether the project has those arms: kd heraldry check \"" + model.HeraldryExample + "\" --project <name>\n" +
		"  3. Give the epic its arms: kd update <id> --heraldry \"" + model.HeraldryExample + "\" (or kd create ... --heraldry)\n\n" +
		"kd show then prints them on one line:\n\n" +
		"  Arms:        " + model.HeraldryExample + "\n\n" +
		model.HeraldryHelp + "\n\n" +
		"What good arms are: gardenio/docs/heraldry/rubric.md. The page: gardenio/docs/heraldry/README.md."
}

var heraldryLibraryCmd = &cobra.Command{
	Use:   "library",
	Short: "List the emoji arms can be made of, with their names",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		word, _ := cmd.Flags().GetString("search")
		charges := libraryMatching(word)
		if jsonOutput {
			type entry struct {
				Emoji string `json:"emoji"`
				Name  string `json:"name"`
			}
			out := make([]entry, len(charges))
			for i, c := range charges {
				out[i] = entry{c.Emoji, c.Name}
			}
			emitJSON(out)
			return nil
		}
		printLibrary(cmd.OutOrStdout(), charges, word)
		return nil
	},
}

// libraryMatching is the library's charges whose name holds word, in any
// case; every charge for "".
func libraryMatching(word string) []heraldry.Charge {
	word = strings.ToLower(strings.TrimSpace(word))
	if word == "" {
		return heraldry.Library()
	}
	var out []heraldry.Charge
	for _, c := range heraldry.Library() {
		if strings.Contains(c.Name, word) {
			out = append(out, c)
		}
	}
	return out
}

func printLibrary(out io.Writer, charges []heraldry.Charge, word string) {
	for _, c := range charges {
		// Pad by width on screen, so the names line up however wide the
		// terminal draws the emoji.
		pad := 3 - uniseg.StringWidth(c.Emoji)
		if pad < 1 {
			pad = 1
		}
		fmt.Fprintf(out, "%s%s%s\n", c.Emoji, strings.Repeat(" ", pad), c.Name)
	}
	switch {
	case word == "":
		fmt.Fprintf(out, "\n%d emoji\n", len(charges))
	case len(charges) == 0:
		fmt.Fprintf(out, "No emoji in the library has %q in its name.\n", word)
	default:
		fmt.Fprintf(out, "\n%d emoji with %q in the name\n", len(charges), word)
	}
}

var heraldryCheckCmd = &cobra.Command{
	Use:   "check <emoji…>",
	Short: "Check arms before you give them: the run, and whether the project already has them",
	Long: "Check arms before giving them to a bead: kd heraldry check " + model.HeraldryExample + " prints the run and its name in words, " +
		"and whether another bead of the project already bears the same run or the same emoji in another order. " +
		"It exits non-zero when the arms are invalid or already taken.\n\n" + model.HeraldryHelp,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		raw := strings.Join(args, " ")
		if err := checkArmsFlag(raw); err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(raw), armsNone) {
			return errors.New("'none' removes arms; give three to five emoji to check")
		}
		arms, _ := heraldry.Parse(raw)
		var register []*model.Bead
		if project != "" {
			// The label filter as well, so a daemon that predates
			// heraldry_project still reads only the project's beads.
			resp, err := beadsClient.ListBeads(context.Background(), &client.ListBeadsRequest{
				HeraldryProject: project,
				Labels:          []string{"project:" + project},
				Limit:           armsRegisterLimit,
			})
			if err != nil {
				return fmt.Errorf("reading the arms already given in project %s: %w", project, err)
			}
			register = resp.Beads
		}
		report := checkArms(arms, project, register)
		if jsonOutput {
			emitJSON(report)
		} else {
			printArmsReport(cmd.OutOrStdout(), report)
		}
		if report.Taken {
			return errors.New(report.TakenWords)
		}
		return nil
	},
}

// armsRegisterLimit bounds the register read; the daemon's own read uses the
// same bound (heraldryRegisterLimit).
const armsRegisterLimit = 5000

// armsBearer is a bead of the register, as the check names it.
type armsBearer struct {
	ID         string `json:"id"`
	ShortTitle string `json:"short_title,omitempty"`
	Arms       string `json:"arms"` // the run: "🔥🧭📦"
}

func (b armsBearer) String() string {
	if b.ShortTitle == "" {
		return b.ID
	}
	return b.ID + " (" + b.ShortTitle + ")"
}

// armsReport is what `kd heraldry check` found. Every string is plain words
// or emoji.
type armsReport struct {
	Arms    string `json:"arms"`  // the run: "🔥🧭📦"
	Words   string `json:"words"` // the plain-words name (§8)
	Project string `json:"project,omitempty"`
	// Taken is true when another bead of the project bears the same run.
	Taken      bool        `json:"taken"`
	TakenBy    *armsBearer `json:"taken_by,omitempty"`
	TakenWords string      `json:"taken_words,omitempty"`
	// SameEmoji are beads with the same emoji in another order: allowed,
	// with a warning, as they may be hard to tell apart.
	SameEmoji []armsBearer `json:"same_emoji_other_order,omitempty"`
}

// checkArms is the daemon's register check (server checkHeraldry, R5) run on
// the client against register, the beads bearing arms in project: whether
// the run is taken, and the look-alikes the daemon would warn about. With
// project "" there is no register.
func checkArms(arms heraldry.Arms, project string, register []*model.Bead) armsReport {
	run, set := heraldry.Run(arms), heraldry.EmojiSet(arms)
	r := armsReport{Arms: run, Words: heraldry.PlainName(arms), Project: project}
	for _, b := range register {
		other, ok := parseStoredArms(b.Heraldry)
		if !ok {
			continue
		}
		bearer := armsBearer{ID: b.ID, ShortTitle: b.ShortTitle, Arms: heraldry.Run(other)}
		switch {
		case bearer.Arms == run && !r.Taken:
			r.Taken, r.TakenBy = true, &bearer
			r.TakenWords = fmt.Sprintf("the arms %s are already borne by %s in project %s; arms are unique in a project, so choose other emoji or another order",
				run, bearer, project)
		case bearer.Arms != run && heraldry.EmojiSet(other) == set:
			r.SameEmoji = append(r.SameEmoji, bearer)
		}
	}
	return r
}

func printArmsReport(out io.Writer, r armsReport) {
	fmt.Fprintf(out, "Arms:       %s\n", r.Arms)
	fmt.Fprintf(out, "In words:   %s\n", r.Words)
	switch {
	case r.Project == "":
		fmt.Fprintln(out, "Project:    none given, so nothing was checked against other beads; add --project <name>")
	case r.Taken:
		fmt.Fprintf(out, "Project:    %s: taken by %s\n", r.Project, r.TakenBy)
	default:
		fmt.Fprintf(out, "Project:    %s: free, no other bead bears these arms\n", r.Project)
	}
	for _, b := range r.SameEmoji {
		fmt.Fprintf(out, "Looks like: %s, %s: the same emoji, in another order\n", b, b.Arms)
	}
}

func init() {
	heraldryLibraryCmd.Flags().String("search", "", "list only the emoji whose name holds this word (fire, key, moon)")
	heraldryCheckCmd.Flags().String("project", defaultProject(), "project whose beads to check the arms against (default: $KD_PROJECT or $BOAT_PROJECT)")
	heraldryCmd.AddCommand(heraldryLibraryCmd)
	heraldryCmd.AddCommand(heraldryCheckCmd)
}

// armsHeader is the ARMS header cell and its tab for the search tables, ""
// when no row bears arms.
func armsHeader(show bool) string {
	if !show {
		return ""
	}
	return "ARMS\t"
}

// armsCell is a row's ARMS cell and its tab for the search tables, "" when
// the table has no ARMS column. The arms sit before the title (§4.2), as the
// search tables carry no short title.
func armsCell(wide *wideCells, show bool, b *model.Bead) string {
	if !show {
		return ""
	}
	return wide.cell(armsRun(b.Heraldry)) + "\t"
}
