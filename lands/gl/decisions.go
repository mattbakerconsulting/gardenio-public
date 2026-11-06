package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

func newDecisionCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "decision",
		Aliases: []string{"decisions", "decide"},
		Short:   "Questions put to the land: list, show, resolve, cancel, comment",
	}
	cmd.AddCommand(decisionList(a), decisionShow(a), decisionResolve(a), decisionCancel(a), decisionComment(a))
	return cmd
}

func decisionList(a *app) *cobra.Command {
	var pg paging
	var status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List decisions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			p := &landsv1.ListDecisionsParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()}
			if status != "" {
				st := landsv1.DecisionStatus(status)
				p.Status = &st
			}
			r, err := l.c.ListDecisionsWithResponse(cmd.Context(), l.land, p)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			pg := r.JSON200
			err = a.emit(pg, func(w io.Writer) {
				if len(pg.Items) == 0 {
					fmt.Fprintln(w, "No decisions.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tSTATUS\tOPENED\tQUESTION")
				for _, d := range pg.Items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Id, d.Status, stamp(d.OpenedAt), oneLine(d.Question))
				}
				tw.Flush()
			})
			a.moreHint(pg.NextCursor)
			return err
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "only decisions in this status: open, resolved or cancelled")
	pg.flags(cmd)
	return cmd
}

func decisionShow(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a decision and its options",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			d, err := l.decision(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.decision(d)
		},
	}
}

func (l *landConn) decision(ctx context.Context, id string) (*landsv1.Decision, error) {
	r, err := l.c.GetDecisionWithResponse(ctx, l.land, id)
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
	}
	return r.JSON200, nil
}

func (a *app) decision(d *landsv1.Decision) error {
	return a.emit(d, func(w io.Writer) {
		fmt.Fprintf(w, "%s  %s\n", d.Id, d.Question)
		fmt.Fprintf(w, "Status: %s\n", d.Status)
		fmt.Fprintf(w, "Opened: %s\n", stamp(d.OpenedAt))
		if d.Context != nil && *d.Context != "" {
			fmt.Fprintf(w, "\n%s\n", strings.TrimRight(*d.Context, "\n"))
		}
		fmt.Fprintln(w, "\nOptions:")
		for i, o := range d.Options {
			mark := " "
			if d.Chosen != nil && *d.Chosen == o.Id {
				mark = "*"
			}
			fmt.Fprintf(w, " %s %d. %s  [%s]\n", mark, i+1, o.Label, o.Id)
		}
	})
}

// pickOption finds the option the person meant: its id, its number in the
// list (from 1), or its label.
func pickOption(d *landsv1.Decision, want string) (string, error) {
	for _, o := range d.Options {
		if o.Id == want {
			return o.Id, nil
		}
	}
	if n, err := strconv.Atoi(want); err == nil && n >= 1 && n <= len(d.Options) {
		return d.Options[n-1].Id, nil
	}
	for _, o := range d.Options {
		if strings.EqualFold(strings.TrimSpace(o.Label), strings.TrimSpace(want)) {
			return o.Id, nil
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q is not one of the options of %s:", want, d.Id)
	for i, o := range d.Options {
		fmt.Fprintf(&b, "\n  %d. %s  [%s]", i+1, o.Label, o.Id)
	}
	return "", fmt.Errorf("%s", b.String())
}

func decisionResolve(a *app) *cobra.Command {
	var option, note string
	cmd := &cobra.Command{
		Use:   "resolve <id> --option <option>",
		Short: "Answer a decision",
		Long:  "Answer a decision. --option takes the option's id, its number in 'gl decision show', or its label.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			d, err := l.decision(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			opt, err := pickOption(d, option)
			if err != nil {
				return err
			}
			b := landsv1.DecisionResolve{OptionId: opt}
			if note != "" {
				b.Note = &note
			}
			r, err := l.c.ResolveDecisionWithResponse(cmd.Context(), l.land, d.Id, &landsv1.ResolveDecisionParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.decision(r.JSON200)
		},
	}
	cmd.Flags().StringVar(&option, "option", "", "the option you choose (id, number or label)")
	cmd.Flags().StringVar(&note, "note", "", "a note to go with your answer")
	_ = cmd.MarkFlagRequired("option")
	return cmd
}

func decisionCancel(a *app) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "cancel <id> --reason <why>",
		Short: "Withdraw a decision without answering it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.CancelDecisionWithResponse(cmd.Context(), l.land, args[0], &landsv1.CancelDecisionParams{IdempotencyKey: newKey()}, landsv1.DecisionCancel{Reason: reason})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.decision(r.JSON200)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why it no longer needs an answer")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func decisionComment(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "comment <id> <text>...",
		Short: "Comment on a decision",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			b := landsv1.CommentCreate{Text: strings.Join(args[1:], " ")}
			r, err := l.c.CommentDecisionWithResponse(cmd.Context(), l.land, args[0], &landsv1.CommentDecisionParams{IdempotencyKey: newKey()}, b)
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
