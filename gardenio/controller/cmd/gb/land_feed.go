package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gardenio/clicreds/oidclogin"
)

// The land feed (D.18b gd-jHjROHhaqz). A land door URL
// (https://<host>/land/<name>) does not serve the daemon's
// /v1/events/stream (D.2), so gb's waits (gb yield, gb decision create
// without --no-wait) consume the land's own event feed instead: GET
// /lands/v1/lands/{land}/events (D.9), which the door serves at
// /land/<name>/v1/lands/v1/lands/<name>/events. The feed ends each stream
// after the access-token lifetime (5 minutes) or when a reader falls behind;
// the reader reconnects with a fresh bearer and its last cursor
// (Last-Event-ID), and the server replays what it missed.

// Land feed event types (the contract's EventType) that gb's waits act on.
const (
	landEventDecisionResolved = "decision.resolved"
	landEventTicketUpdated    = "ticket.updated"
)

const (
	// landFeedRetries is how many reconnects in a row may fail before the
	// wait gives up on the feed and falls back to polling.
	landFeedRetries = 3
	// landFeedGuardPoll is the check the waits keep running beside the feed:
	// the feed files a bead under a land by its project:<land> label and does
	// not stream mail, so a bead without the label still wakes the wait, a
	// little later.
	landFeedGuardPoll = 30 * time.Second
)

// landFeedBackoff is the wait before the first reconnect retry; it doubles
// on each further one. A var so tests need not sleep.
var landFeedBackoff = 2 * time.Second

// landDoor returns the land gb's daemon URL goes through the land door for,
// or "" when it is not a land door.
func landDoor() string {
	if daemon == nil {
		return ""
	}
	return oidclogin.LandFromURL(daemon.BaseURL())
}

// landEvent is one frame of the land feed: its event type and the record it
// carries (a Ticket, Decision, ... in the Lands contract).
type landEvent struct {
	Type string
	Data json.RawMessage
}

// landRecord is the part of a feed record the waits read.
type landRecord struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Assignee string `json:"assignee"`
}

// streamLandFeed opens land's event feed. The returned channel stays open
// across stream ends, reconnecting from the last cursor, and closes when ctx
// ends or landFeedRetries reconnects in a row fail. An error means the first
// connect failed and the caller should poll.
func streamLandFeed(ctx context.Context, land string) (<-chan landEvent, error) {
	// No client timeout: the stream is long-lived, and ctx ends it.
	hc := &http.Client{}
	body, err := openLandFeed(ctx, hc, land, "")
	if err != nil {
		return nil, err
	}
	ch := make(chan landEvent, 16)
	go func() {
		defer close(ch)
		cursor := ""
		for {
			cursor = readLandFeed(ctx, body, cursor, ch)
			body.Close()
			for attempt := 0; ; attempt++ {
				if ctx.Err() != nil {
					return
				}
				if body, err = openLandFeed(ctx, hc, land, cursor); err == nil {
					break
				}
				if attempt+1 >= landFeedRetries {
					fmt.Fprintf(os.Stderr, "Warning: land feed for %s: %v; polling instead\n", land, err)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(landFeedBackoff << attempt):
				}
			}
		}
	}()
	return ch, nil
}

// openLandFeed connects to land's feed, resuming after cursor when it is set.
func openLandFeed(ctx context.Context, hc *http.Client, land, cursor string) (io.ReadCloser, error) {
	u := daemon.BaseURL() + "/v1/lands/v1/lands/" + url.PathEscape(land) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	// Each connect takes the bearer afresh: a stream lasts as long as the
	// access token that opened it, and the login session refreshes it.
	if tok := resolveBearer(ctx, os.Stderr); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("land feed returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// readLandFeed sends body's frames to ch until the stream ends, and returns
// the cursor to resume from: the last frame's id, or cursor if none came.
func readLandFeed(ctx context.Context, body io.Reader, cursor string, ch chan<- landEvent) string {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var id, typ string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				select {
				case ch <- landEvent{Type: typ, Data: json.RawMessage(strings.Join(data, "\n"))}:
				case <-ctx.Done():
					return cursor
				}
				if id != "" {
					cursor = id
				}
			}
			id, typ, data = "", "", nil
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "event":
			typ = value
		case "data":
			data = append(data, value)
		}
	}
	return cursor
}

// landDisplayName is how the feed shows an assignee: the land map's
// displayName (kbeads internal/landmap), which drops a "<provider>:" prefix
// and an "@domain".
func landDisplayName(s string) string {
	s = strings.TrimSpace(s)
	if p, rest, ok := strings.Cut(s, ":"); ok && p != "" && !strings.ContainsAny(p, "@ ") {
		s = rest
	}
	if local, _, ok := strings.Cut(s, "@"); ok {
		s = local
	}
	return s
}
