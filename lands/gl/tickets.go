package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

func newTicketCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ticket",
		Aliases: []string{"tickets"},
		Short:   "The land's tickets: list, show, open, search, comment, close, reopen",
	}
	cmd.AddCommand(ticketList(a), ticketShow(a), ticketOpen(a), ticketSearch(a), ticketComment(a), ticketClose(a), ticketReopen(a))
	return cmd
}

func ticketList(a *app) *cobra.Command {
	var pg paging
	var status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tickets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			p := &landsv1.ListTicketsParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()}
			if status != "" {
				st := landsv1.TicketStatus(status)
				p.Status = &st
			}
			r, err := l.c.ListTicketsWithResponse(cmd.Context(), l.land, p)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.ticketPage(r.JSON200)
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "only tickets in this status: open, in_progress, waiting or done")
	pg.flags(cmd)
	return cmd
}

func ticketSearch(a *app) *cobra.Command {
	var pg paging
	cmd := &cobra.Command{
		Use:   "search <text>...",
		Short: "Search the land's tickets",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			p := &landsv1.SearchTicketsParams{Q: strings.Join(args, " "), Cursor: pg.cursorPtr(), Limit: pg.limitPtr()}
			r, err := l.c.SearchTicketsWithResponse(cmd.Context(), l.land, p)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.ticketPage(r.JSON200)
		},
	}
	pg.flags(cmd)
	return cmd
}

func (a *app) ticketPage(pg *landsv1.TicketPage) error {
	err := a.emit(pg, func(w io.Writer) {
		if len(pg.Items) == 0 {
			fmt.Fprintln(w, "No tickets.")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tP\tKIND\tTITLE")
		for _, t := range pg.Items {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", t.Id, t.Status, t.Priority, t.Kind, titled(t.Heraldry, t.Title))
		}
		tw.Flush()
	})
	a.moreHint(pg.NextCursor)
	return err
}

func ticketShow(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.GetTicketWithResponse(cmd.Context(), l.land, args[0])
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.ticket(r.JSON200)
		},
	}
}

func (a *app) ticket(t *landsv1.Ticket) error {
	return a.emit(t, func(w io.Writer) {
		fmt.Fprintf(w, "%s  %s\n", t.Id, titled(t.Heraldry, t.Title))
		fmt.Fprintf(w, "Kind:     %s\n", t.Kind)
		fmt.Fprintf(w, "Status:   %s\n", t.Status)
		fmt.Fprintf(w, "Priority: %d\n", t.Priority)
		if t.Assignee != nil {
			fmt.Fprintf(w, "Assignee: %s\n", *t.Assignee)
		}
		fmt.Fprintf(w, "Updated:  %s\n", stamp(t.UpdatedAt))
		if t.Body != nil && *t.Body != "" {
			fmt.Fprintf(w, "\n%s\n", strings.TrimRight(*t.Body, "\n"))
		}
	})
}

func ticketOpen(a *app) *cobra.Command {
	var kind, body string
	var priority int
	cmd := &cobra.Command{
		Use:     "open <title>...",
		Aliases: []string{"create"},
		Short:   "Open a new ticket",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			b := landsv1.TicketCreate{Kind: landsv1.TicketKind(kind), Title: strings.Join(args, " ")}
			if body != "" {
				b.Body = &body
			}
			if cmd.Flags().Changed("priority") {
				b.Priority = &priority
			}
			r, err := l.c.CreateTicketWithResponse(cmd.Context(), l.land, &landsv1.CreateTicketParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON201 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.ticket(r.JSON201)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", string(landsv1.Task), "task, bug or feature")
	cmd.Flags().StringVar(&body, "body", "", "what the ticket is about")
	cmd.Flags().IntVar(&priority, "priority", 0, "1 (most urgent) to 4")
	return cmd
}

func ticketComment(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "comment <id> <text>...",
		Short: "Comment on a ticket",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			b := landsv1.CommentCreate{Text: strings.Join(args[1:], " ")}
			r, err := l.c.CommentTicketWithResponse(cmd.Context(), l.land, args[0], &landsv1.CommentTicketParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON201 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.comment(r.JSON201)
		},
	}
}

func (a *app) comment(c *landsv1.Comment) error {
	return a.emit(c, func(w io.Writer) {
		fmt.Fprintf(w, "Commented as %s (%s).\n", c.Author, c.Id)
	})
}

func ticketClose(a *app) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "close <id>",
		Short: "Close a ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			b := landsv1.TicketClose{}
			if reason != "" {
				b.Reason = &reason
			}
			r, err := l.c.CloseTicketWithResponse(cmd.Context(), l.land, args[0], &landsv1.CloseTicketParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.ticket(r.JSON200)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why it is done")
	return cmd
}

func ticketReopen(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "reopen <id>",
		Short: "Reopen a closed ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.ReopenTicketWithResponse(cmd.Context(), l.land, args[0], &landsv1.ReopenTicketParams{IdempotencyKey: newKey()})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.ticket(r.JSON200)
		},
	}
}

// oneLine is s on one line, for a table cell.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// titled is a title on one line with the record's arms before it as their
// emoji run ("🔥🧭📦 Autumn shop"), the arms' form in a terminal.
func titled(arms *landsv1.Heraldry, title string) string {
	if run := emojiRun(arms); run != "" {
		return run + " " + oneLine(title)
	}
	return oneLine(title)
}

// emojiRun is the arms as an emoji run with nothing between the emoji. The
// contract keeps heraldry opaque; gl, shipped with the contract, keeps every
// token that is not plain ASCII, so the run (🔥🧭📦) prints as it is and a
// value from before arms were a run (a colour word, then five places, "."
// for an empty one) prints as its run, never its colour or empty places.
func emojiRun(arms *landsv1.Heraldry) string {
	if arms == nil {
		return ""
	}
	var sb strings.Builder
	for _, tok := range strings.Fields(*arms) {
		if !isASCII(tok) {
			sb.WriteString(tok)
		}
	}
	return sb.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
