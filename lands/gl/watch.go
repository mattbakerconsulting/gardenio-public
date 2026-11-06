package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

// The live feed (GET /lands/v1/lands/<land>/events) is server-sent events.
// The land ends every stream after a few minutes; gl reconnects with a fresh
// bearer and the last event's id, so nothing is missed or repeated. A 429
// (the land's stream cap or new-stream rate) and an outage back off and
// retry.

const (
	watchBackoffMin = time.Second
	watchBackoffMax = 30 * time.Second
	// A stream that ends sooner than this with no events is treated as a
	// failure and backed off, so a land that hangs up at once is not
	// hammered.
	watchMinStream = 2 * time.Second
)

func newWatchCmd(a *app) *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Follow the land's live feed",
		Long: `Follow the land's live feed: ticket, decision, escalation, plan, message,
budget and release changes as they happen, one per line (--json: one event
object per line). Each line ends with the event's cursor; --since <cursor> resumes from
it. Stop with Ctrl-C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			return l.watch(cmd.Context(), since)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "resume after this event cursor")
	return cmd
}

func (l *landConn) watch(ctx context.Context, cursor string) error {
	d := &doer{l: l, hc: l.a.streamHC}
	backoff := watchBackoffMin
	wait := func(why string) bool {
		fmt.Fprintf(l.a.errOut, "gl: %s; reconnecting in %s\n", why, backoff)
		l.a.sleep(backoff)
		backoff = min(backoff*2, watchBackoffMax)
		return ctx.Err() == nil
	}
	for ctx.Err() == nil {
		// The generated client resolves its paths against base + "/".
		req, err := landsv1.NewStreamEventsRequest(l.base+"/", l.land, nil)
		if err != nil {
			return err
		}
		req = req.WithContext(ctx)
		req.Header.Set("Accept", "text/event-stream")
		if cursor != "" {
			req.Header.Set("Last-Event-ID", cursor)
		}
		start := l.a.now()
		resp, err := d.Do(req)
		if err != nil {
			var se *signInError
			if ctx.Err() != nil {
				return nil
			}
			if errors.As(err, &se) {
				return err
			}
			if !wait("lost the feed (" + err.Error() + ")") {
				return nil
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			err := l.streamFailure(resp)
			resp.Body.Close()
			var pe *problemError
			retry := resp.StatusCode >= 500 || (errors.As(err, &pe) && (pe.p.Code == landsv1.RateLimited || pe.p.Code == landsv1.UpstreamUnavailable))
			if !retry {
				return err
			}
			if !wait(err.Error()) {
				return nil
			}
			continue
		}
		n, err := readEvents(resp.Body, func(ev rawEvent) error {
			if ev.id != "" {
				cursor = ev.id
			}
			return l.a.printEvent(ev)
		})
		resp.Body.Close()
		if ctx.Err() != nil {
			return nil
		}
		var pe printError
		if errors.As(err, &pe) {
			return pe.error
		}
		if n > 0 {
			backoff = watchBackoffMin
		}
		if n == 0 && l.a.now().Sub(start) < watchMinStream {
			if !wait("the feed closed at once") {
				return nil
			}
		}
	}
	return nil
}

func (l *landConn) streamFailure(resp *http.Response) error {
	var p landsv1.Problem
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(body, &p) == nil {
			return l.fail(resp.StatusCode, &p)
		}
	}
	return l.fail(resp.StatusCode, nil)
}

// rawEvent is one SSE frame.
type rawEvent struct {
	id, typ, data string
}

// printError is a failure writing an event out, which ends the watch.
type printError struct{ error }

// readEvents reads SSE frames from r until it ends, calling fn for each
// frame that carries data, and returns how many it delivered. Comment lines
// (keep-alives) and retry: are ignored.
func readEvents(r io.Reader, fn func(rawEvent) error) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	var ev rawEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				ev.data = strings.Join(data, "\n")
				if err := fn(ev); err != nil {
					return n, printError{err}
				}
				n++
			}
			ev, data = rawEvent{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.id = value
		case "event":
			ev.typ = value
		case "data":
			data = append(data, value)
		}
	}
	return n, sc.Err()
}

// printEvent writes one event: a JSON object per line with --json, else a
// line a person can read.
func (a *app) printEvent(ev rawEvent) error {
	if a.jsonOut {
		var obj json.RawMessage = []byte(ev.data)
		if !json.Valid(obj) {
			return nil // a frame that is not JSON is not a contract event
		}
		line, err := json.Marshal(struct {
			Id     string          `json:"id"`
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}{ev.id, ev.typ, obj})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.out, "%s\n", line)
		return err
	}
	_, err := fmt.Fprintf(a.out, "%s  %s  [%s]\n", ev.typ, describeEvent(ev), ev.id)
	return err
}

// describeEvent is the one-line summary of an event's object. An event type
// this gl does not know is printed by its type alone.
func describeEvent(ev rawEvent) string {
	raw := []byte(ev.data)
	switch landsv1.EventType(ev.typ) {
	case landsv1.TicketUpdated:
		var t landsv1.Ticket
		if json.Unmarshal(raw, &t) == nil {
			return fmt.Sprintf("%s %s: %s", t.Id, t.Status, oneLine(t.Title))
		}
	case landsv1.DecisionOpened, landsv1.DecisionResolved:
		var d landsv1.Decision
		if json.Unmarshal(raw, &d) == nil {
			return fmt.Sprintf("%s %s: %s", d.Id, d.Status, oneLine(d.Question))
		}
	case landsv1.EscalationOpened:
		var e landsv1.Escalation
		if json.Unmarshal(raw, &e) == nil {
			return fmt.Sprintf("%s %s: %s", e.Id, e.Status, oneLine(e.Title))
		}
	case landsv1.MessagePosted:
		var m landsv1.Message
		if json.Unmarshal(raw, &m) == nil {
			return fmt.Sprintf("%s %s: %s", m.ThreadId, m.Author, clip(oneLine(m.Text), 120))
		}
	case landsv1.PlanUpdated:
		var p landsv1.Plan
		if json.Unmarshal(raw, &p) == nil {
			return fmt.Sprintf("%s %s (%s done): %s", p.Id, p.State, progress(p.Steps), oneLine(p.Title))
		}
	case landsv1.BudgetUpdated:
		var b landsv1.Budget
		if json.Unmarshal(raw, &b) == nil {
			return fmt.Sprintf("spent %s of %s %s (%s)", amount(b.Spent), amount(b.Limit), b.Unit, b.Status)
		}
	case landsv1.ReleaseCut, landsv1.ReleaseDeployed:
		var r landsv1.Release
		if json.Unmarshal(raw, &r) == nil {
			return fmt.Sprintf("%s %s, %d pull request%s, runs on %s", r.Tag, r.Status, len(r.Contents), plural(len(r.Contents)), runsOn(r.Deployments))
		}
	}
	return "(other)"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
