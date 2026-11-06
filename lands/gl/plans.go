package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

func newPlanCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "plan",
		Aliases: []string{"plans"},
		Short:   "The land's plans: list, show, answer",
	}
	cmd.AddCommand(planList(a), planShow(a), planAnswer(a))
	return cmd
}

func planList(a *app) *cobra.Command {
	var pg paging
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List plans",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.ListPlansWithResponse(cmd.Context(), l.land, &landsv1.ListPlansParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			pg := r.JSON200
			err = a.emit(pg, func(w io.Writer) {
				if len(pg.Items) == 0 {
					fmt.Fprintln(w, "No plans.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tSTATE\tDONE\tTITLE")
				for _, p := range pg.Items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Id, p.State, progress(p.Steps), titled(p.Heraldry, p.Title))
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

func progress(steps []landsv1.PlanStep) string {
	done := 0
	for _, s := range steps {
		if s.Status == landsv1.TicketStatusDone {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(steps))
}

func planShow(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a plan and its steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.GetPlanWithResponse(cmd.Context(), l.land, args[0])
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			p := r.JSON200
			return a.emit(p, func(w io.Writer) {
				fmt.Fprintf(w, "%s  %s\n", p.Id, titled(p.Heraldry, p.Title))
				fmt.Fprintf(w, "State: %s (%s done)\n", p.State, progress(p.Steps))
				if p.Phase != nil {
					fmt.Fprintf(w, "Phase: %s\n", *p.Phase)
				}
				if len(p.Steps) > 0 {
					fmt.Fprintln(w)
					tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
					for _, s := range p.Steps {
						fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Id, s.Status, titled(s.Heraldry, s.Title))
					}
					tw.Flush()
				}
			})
		},
	}
}

func planAnswer(a *app) *cobra.Command {
	var decision, option, note string
	cmd := &cobra.Command{
		Use:   "answer <plan> --decision <id> --option <option>",
		Short: "Answer one of a plan's open questions",
		Long:  "Answer one of a plan's open questions. --option takes the option's id, its number in 'gl decision show', or its label.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			d, err := l.decision(cmd.Context(), decision)
			if err != nil {
				return err
			}
			opt, err := pickOption(d, option)
			if err != nil {
				return err
			}
			b := landsv1.PlanAnswer{DecisionId: d.Id, OptionId: opt}
			if note != "" {
				b.Note = &note
			}
			r, err := l.c.AnswerPlanWithResponse(cmd.Context(), l.land, args[0], &landsv1.AnswerPlanParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.decision(r.JSON200)
		},
	}
	cmd.Flags().StringVar(&decision, "decision", "", "the plan's question you are answering")
	cmd.Flags().StringVar(&option, "option", "", "the option you choose (id, number or label)")
	cmd.Flags().StringVar(&note, "note", "", "a note to go with your answer")
	_ = cmd.MarkFlagRequired("decision")
	_ = cmd.MarkFlagRequired("option")
	return cmd
}
