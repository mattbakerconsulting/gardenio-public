package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

func newThreadCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "thread",
		Aliases: []string{"threads"},
		Short:   "The land's conversations: list, show (post with 'gl say')",
	}
	cmd.AddCommand(threadList(a), threadShow(a))
	return cmd
}

func threadList(a *app) *cobra.Command {
	var pg paging
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List threads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.ListThreadsWithResponse(cmd.Context(), l.land, &landsv1.ListThreadsParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			pg := r.JSON200
			err = a.emit(pg, func(w io.Writer) {
				if len(pg.Items) == 0 {
					fmt.Fprintln(w, "No threads.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tUPDATED\tTITLE")
				for _, t := range pg.Items {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Id, stamp(t.UpdatedAt), oneLine(t.Title))
				}
				tw.Flush()
			})
			a.moreHint(pg.NextCursor)
			return err
		},
	}
	pg.flags(cmd)
	return cmd
}

// threadView is a thread with a page of its messages, for --json.
type threadView struct {
	Thread   *landsv1.Thread      `json:"thread"`
	Messages *landsv1.MessagePage `json:"messages"`
}

func threadShow(a *app) *cobra.Command {
	var pg paging
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a thread's messages, oldest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			tr, err := l.c.GetThreadWithResponse(cmd.Context(), l.land, args[0])
			if err != nil {
				return err
			}
			if tr.JSON200 == nil {
				return l.fail(tr.StatusCode(), tr.ApplicationproblemJSONDefault)
			}
			mr, err := l.c.ListMessagesWithResponse(cmd.Context(), l.land, args[0], &landsv1.ListMessagesParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()})
			if err != nil {
				return err
			}
			if mr.JSON200 == nil {
				return l.fail(mr.StatusCode(), mr.ApplicationproblemJSONDefault)
			}
			t, msgs := tr.JSON200, mr.JSON200
			err = a.emit(threadView{Thread: t, Messages: msgs}, func(w io.Writer) {
				fmt.Fprintf(w, "%s  %s\n", t.Id, t.Title)
				for _, m := range msgs.Items {
					fmt.Fprintf(w, "\n%s  %s\n%s\n", m.Author, stamp(m.PostedAt), strings.TrimRight(m.Text, "\n"))
				}
			})
			a.moreHint(msgs.NextCursor)
			return err
		},
	}
	pg.flags(cmd)
	return cmd
}

func newSayCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "say <thread> <text>...",
		Short: "Post a message to a thread",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			b := landsv1.MessageCreate{Text: strings.Join(args[1:], " ")}
			r, err := l.c.PostMessageWithResponse(cmd.Context(), l.land, args[0], &landsv1.PostMessageParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON201 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			m := r.JSON201
			return a.emit(m, func(w io.Writer) {
				fmt.Fprintf(w, "Posted to %s as %s (%s).\n", m.ThreadId, m.Author, m.Id)
			})
		},
	}
}

func newBudgetCmd(a *app) *cobra.Command {
	var history bool
	var days int
	cmd := &cobra.Command{
		Use:   "budget",
		Short: "What the land may spend and has spent",
		Long: `What the land may spend and has spent. With --history, the credits spent
each UTC day and what remained at its end, oldest first (--days, 1 to 30). A
history marked stale comes from the purse's own snapshots, because the credit
series could not be read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !history && cmd.Flags().Changed("days") {
				return errors.New("--days goes with --history")
			}
			l, err := a.connect()
			if err != nil {
				return err
			}
			if history {
				return budgetHistory(cmd, a, l, days)
			}
			r, err := l.c.GetBudgetWithResponse(cmd.Context(), l.land)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			b := r.JSON200
			return a.emit(b, func(w io.Writer) {
				fmt.Fprintf(w, "Spent %s of %s %s (%s).\n", amount(b.Spent), amount(b.Limit), b.Unit, b.Status)
			})
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "the credits spent and remaining each UTC day")
	cmd.Flags().IntVar(&days, "days", 30, "with --history: how many days back, today included (1 to 30)")
	return cmd
}

func budgetHistory(cmd *cobra.Command, a *app, l *landConn, days int) error {
	if days < 1 || days > 30 {
		return errors.New("--days must be 1 to 30")
	}
	r, err := l.c.GetBudgetHistoryWithResponse(cmd.Context(), l.land, &landsv1.GetBudgetHistoryParams{Days: &days})
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
	}
	h := r.JSON200
	return a.emit(h, func(w io.Writer) {
		if len(h.Points) == 0 {
			fmt.Fprintln(w, "No credit history yet.")
		} else {
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "DAY\tSPENT (%[1]s)\tREMAINING (%[1]s)\n", h.Unit)
			for _, p := range h.Points {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Day.Format("2006-01-02"), amount(p.Spent), amount(p.Remaining))
			}
			_ = tw.Flush()
		}
		if h.Stale != nil && *h.Stale {
			fmt.Fprintln(w, "Stale: the credit series could not be read; these days are from the purse's own snapshots.")
		}
	})
}

func amount(f float32) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".")
}
