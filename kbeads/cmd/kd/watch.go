package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:     "watch <view-name>",
	Short:   "Watch for beads matching a saved view",
	GroupID: "views",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		interval, _ := cmd.Flags().GetDuration("interval")
		once, _ := cmd.Flags().GetBool("once")

		// 1. Fetch the view config.
		cfg, err := resolveConfigBead(context.Background(), "view:"+name)
		if err != nil {
			return fmt.Errorf("getting view config %q: %w", name, err)
		}

		var vc viewConfig
		if err := json.Unmarshal(cfg.Value, &vc); err != nil {
			return fmt.Errorf("parsing view config %q: %w", name, err)
		}

		// 2. Build the ListBeads request.
		req := &client.ListBeadsRequest{
			Status:   vc.Filter.Status,
			Type:     vc.Filter.Type,
			Kind:     vc.Filter.Kind,
			Labels:   vc.Filter.Labels,
			Assignee: expandVar(vc.Filter.Assignee),
			Search:   vc.Filter.Search,
			Sort:     vc.Sort,
			Limit:    vc.Limit,
			Priority: vc.Filter.Priority,
		}
		if len(vc.Filter.Fields) > 0 {
			req.FieldFilters = vc.Filter.Fields
		}

		// 3. Setup signal handling.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		seen := make(map[string]time.Time)

		// 4. Initial query.
		if err := queryAndPrint(ctx, req, seen); err != nil {
			return err
		}
		if once {
			return nil
		}

		// 5. Choose event-driven or polling mode.
		natsURL := os.Getenv("BEADS_NATS_URL")
		if natsURL == "" {
			natsURL = activeRemoteNATSURL()
		}
		if natsURL != "" {
			return watchNATS(ctx, natsURL, req, seen)
		}
		return watchPoll(ctx, interval, req, seen)
	},
}

// watchNATS subscribes to NATS events and re-queries on changes with debounce.
func watchNATS(ctx context.Context, natsURL string, req *client.ListBeadsRequest, seen map[string]time.Time) error {
	// reconnectCh receives a signal when the NATS client reconnects after
	// a disconnect, so we can immediately re-query for missed events.
	reconnectCh := make(chan struct{}, 1)

	opts := append(events.AuthOptions(os.Getenv("BEADS_NATS_USER"), os.Getenv("BEADS_NATS_TOKEN")),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Printf("nats: disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			log.Printf("nats: reconnected")
			select {
			case reconnectCh <- struct{}{}:
			default:
			}
		}),
	)
	sub, err := events.NewNATSSubscriber(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("connecting to NATS: %w", err)
	}
	defer sub.Close()

	ch, cancel, err := sub.Subscribe("beads.>")
	if err != nil {
		return fmt.Errorf("subscribing to events: %w", err)
	}
	defer cancel()

	debounce := time.NewTimer(0)
	debounce.Stop()
	// Drain the timer channel in case it fired between NewTimer and Stop.
	select {
	case <-debounce.C:
	default:
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ch:
			if !ok {
				return nil
			}
			debounce.Reset(200 * time.Millisecond)
		case <-reconnectCh:
			debounce.Reset(0) // immediate re-query
		case <-debounce.C:
			if err := queryAndPrint(ctx, req, seen); err != nil {
				return err
			}
		}
	}
}

// watchPoll polls for changes at the given interval.
func watchPoll(ctx context.Context, interval time.Duration, req *client.ListBeadsRequest, seen map[string]time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
		if err := queryAndPrint(ctx, req, seen); err != nil {
			return err
		}
	}
}

// queryAndPrint calls ListBeads, diffs against the seen map, and prints any changes.
func queryAndPrint(ctx context.Context, req *client.ListBeadsRequest, seen map[string]time.Time) error {
	changed, total, err := queryAndDiff(ctx, req, seen)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("querying beads: %w", err)
	}
	if len(changed) > 0 {
		if jsonOutput {
			printBeadListJSON(changed)
		} else {
			printBeadListTable(changed, total)
		}
	}
	return nil
}

// queryAndDiff calls ListBeads and returns beads that are new or changed since
// last seen. It updates the seen map in place.
func queryAndDiff(ctx context.Context, req *client.ListBeadsRequest, seen map[string]time.Time) ([]*model.Bead, int, error) {
	resp, err := beadsClient.ListBeads(ctx, req)
	if err != nil {
		return nil, 0, err
	}

	changed := diffBeads(resp.Beads, seen)
	return changed, resp.Total, nil
}

// diffBeads compares beads against the seen map and returns those that are new
// or have a different updated_at timestamp. It updates seen in place.
func diffBeads(beads []*model.Bead, seen map[string]time.Time) []*model.Bead {
	var changed []*model.Bead
	for _, b := range beads {
		prev, ok := seen[b.ID]
		if !ok || !b.UpdatedAt.Equal(prev) {
			changed = append(changed, b)
		}
		seen[b.ID] = b.UpdatedAt
	}
	return changed
}

func init() {
	watchCmd.Flags().Duration("interval", 5*time.Second, "polling interval")
	watchCmd.Flags().Bool("once", false, "exit after first poll")
}
