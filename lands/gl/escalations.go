package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

// Escalations are asks for the land's operator. gl opens and reads them;
// approving or rejecting one happens on its page in the browser, never here
// (design gd-uYNbyW4a9Z section 6 step 7; presence is the page, P10.B
// gd-4mEAhIE01U), so gl prints where, approve and reject included.

func newEscalationCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "escalation",
		Aliases: []string{"escalations", "escalate"},
		Short:   "Ask the land's operator for something: list, show, create",
	}
	cmd.AddCommand(escalationList(a), escalationShow(a), escalationCreate(a), escalationDecide(a, "approve"), escalationDecide(a, "reject"))
	return cmd
}

func escalationList(a *app) *cobra.Command {
	var pg paging
	var status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List escalations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			p := &landsv1.ListEscalationsParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()}
			if status != "" {
				st := landsv1.EscalationStatus(status)
				p.Status = &st
			}
			r, err := l.c.ListEscalationsWithResponse(cmd.Context(), l.land, p)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			pg := r.JSON200
			err = a.emit(pg, func(w io.Writer) {
				if len(pg.Items) == 0 {
					fmt.Fprintln(w, "No escalations.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tSTATUS\tBY\tTITLE")
				for _, e := range pg.Items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Id, e.Status, e.RequestedBy, oneLine(e.Title))
				}
				tw.Flush()
			})
			a.moreHint(pg.NextCursor)
			return err
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "only escalations in this status: open, approved or rejected")
	pg.flags(cmd)
	return cmd
}

func escalationShow(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one escalation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.GetEscalationWithResponse(cmd.Context(), l.land, args[0])
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.escalation(r.JSON200, "")
		},
	}
}

func escalationCreate(a *app) *cobra.Command {
	var title, reason string
	cmd := &cobra.Command{
		Use:   "create --title <title> --reason <why>",
		Short: "Ask the land's operator for something the land cannot do itself",
		Long: `Ask the land's operator for something the land cannot do itself. The
operator approves or rejects it in the browser; gl prints the link.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.CreateEscalationWithResponse(cmd.Context(), l.land, &landsv1.CreateEscalationParams{IdempotencyKey: newKey()}, landsv1.EscalationCreate{Title: title, Reason: reason})
			if err != nil {
				return err
			}
			if r.JSON201 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.escalation(r.JSON201, "Asked. ")
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "what you need, in a line")
	cmd.Flags().StringVar(&reason, "reason", "", "why, and what exactly")
	_ = cmd.MarkFlagRequired("title")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// escalationDecide is approve or reject (verb): gl never decides an escalation,
// so it reads it and hands over its page, opening it with --open. An agent on
// a person's gl sign-in therefore has no way to approve.
func escalationDecide(a *app, verb string) *cobra.Command {
	var open bool
	cmd := &cobra.Command{
		Use:   verb + " <id>",
		Short: "Print the page where the escalation is approved or rejected; gl does neither",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.GetEscalationWithResponse(cmd.Context(), l.land, args[0])
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			e := r.JSON200
			if e.Status != landsv1.EscalationStatusOpen {
				return fmt.Errorf("escalation %s is already %s", e.Id, e.Status)
			}
			if open {
				web, err := a.web()
				if err != nil {
					return err
				}
				if err := a.openBrowser(approvalURL(web, e.Id)); err != nil {
					return fmt.Errorf("opening the page: %w", err)
				}
			}
			return a.escalation(e, "gl does not "+verb+". ")
		},
	}
	cmd.Flags().BoolVar(&open, "open", false, "also open the page in the browser")
	return cmd
}

// escalationView is an escalation with where it is approved, for --json.
type escalationView struct {
	landsv1.Escalation
	ApprovalURL string `json:"approval_url,omitempty"`
}

func (a *app) escalation(e *landsv1.Escalation, lead string) error {
	v := escalationView{Escalation: *e}
	if e.Status == landsv1.EscalationStatusOpen {
		web, err := a.web()
		if err != nil {
			return err
		}
		v.ApprovalURL = approvalURL(web, e.Id)
	}
	return a.emit(v, func(w io.Writer) {
		fmt.Fprintf(w, "%s  %s\n", e.Id, e.Title)
		fmt.Fprintf(w, "Status: %s\n", e.Status)
		fmt.Fprintf(w, "By:     %s\n", e.RequestedBy)
		if e.Reason != "" {
			fmt.Fprintf(w, "\n%s\n", strings.TrimRight(e.Reason, "\n"))
		}
		if v.ApprovalURL != "" {
			fmt.Fprintf(w, "\n%sIt is approved or rejected on its page, in the browser or on a phone:\n  %s\n", lead, v.ApprovalURL)
		}
	})
}
