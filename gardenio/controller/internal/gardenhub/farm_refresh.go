package gardenhub

// farm_refresh.go — when the Farm re-reads (split from farm.go). A work-type
// event marks the farm dirty and, after a settle window, re-reads the rows; an
// agent-only event refreshes just the roster against the last rows (cost plan
// B2); a reconcile tick repairs lost events and forces a full read at least
// every farmFullEvery.

import (
	"context"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub/captioner"
)

const (
	// farmSettle is the trailing window after the first event of a burst (a close
	// is usually a status write plus a dependency write); farmMinGap is the least
	// time between the end of one refresh and the start of the next, which bounds
	// the daemon load under a steady event stream. Worst case an event shows up
	// about farmSettle + farmMinGap + one refresh later, inside the 1-2 s target.
	farmSettle    = 250 * time.Millisecond
	farmMinGap    = 1 * time.Second
	farmReconcile = 60 * time.Second
	// farmFullEvery bounds how stale the work rows get when no work-type event
	// arrives; a roster-only refresh reuses the last rows (cost plan B2).
	farmFullEvery = 5 * time.Minute
	// farmRetryMax caps the retry after failed refreshes: the first is retried
	// after retryAfterError, each further one in a row waits twice as long.
	// A daemon too loaded to answer is not asked again every few seconds.
	farmRetryMax = time.Minute
)

// farmRetryAfter is the wait before retrying the fails-th failed refresh in a row.
func farmRetryAfter(fails int) time.Duration {
	d := retryAfterError
	for i := 1; i < fails && d < farmRetryMax; i++ {
		d *= 2
	}
	return min(d, farmRetryMax)
}

// farmRosterGap is the least time between the end of one refresh and a
// roster-only refresh: agent beads are written about once a minute each (the
// controller's usage writes), and the roster is only shown to the minute. A
// var so tests can shorten it.
var farmRosterGap = 15 * time.Second

// Invalidate marks the farm dirty: the next refresh re-reads everything.
func (f *Farm) Invalidate() {
	select {
	case f.dirty <- struct{}{}:
	default:
	}
}

// InvalidateFor is the Hub's OnEvent hook. A work-type event re-reads the rows;
// an agent event only re-reads the roster and recomposes from the last rows.
func (f *Farm) InvalidateFor(r Relevance) {
	if r.Farm {
		f.Invalidate()
		return
	}
	if r.Roster {
		select {
		case f.rosterDirty <- struct{}{}:
		default:
		}
	}
}

// Start warms the snapshot synchronously (bounded by refreshTimeout), then
// launches the refresher.
func (f *Farm) Start() {
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	f.cancel = cancel
	warm, wcancel := context.WithTimeout(ctx, refreshTimeout)
	if err := f.refresh(warm); err != nil {
		f.logger.Warn("garden farm: initial snapshot failed — will retry", "error", err)
		f.Invalidate()
	}
	wcancel()
	go f.loop(ctx)
	go f.activityLoop(ctx)
}

// Close stops the refresher and closes every subscriber channel.
func (f *Farm) Close() {
	if f == nil || f.cancel == nil {
		return
	}
	f.cancel()
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.closeAll()
	f.act.closeAll()
}

// activityLoop flushes folded activity events (activity.go) until ctx ends.
func (f *Farm) activityLoop(ctx context.Context) {
	t := time.NewTicker(activityTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.act.flush(f.now())
		}
	}
}

// ObserveHook takes one hook relay payload off hooks.<agent>.<event> (cmd/beads-viewer/activity_feed.go). A payload
// the feed does not read is ignored. Nil-safe.
//
// With a captioner (SetCaptioner), a gardener's call start (not a sidechain's, not a pool shell's) has its caption
// counted by source (and, when it has no caption of its own, by tool), and a Bash one with no caption is asked of the
// captioner (spec 8). A cached caption goes on the
// start at once; otherwise the start goes out without one, and a caption Haiku writes later reaches the page as a
// `doing` event (activity.lateCaption, spec 7.2). The captioner never blocks and is never called under a lock.
func (f *Farm) ObserveHook(data []byte) {
	if f == nil {
		return
	}
	now := f.now()
	ev, hb, ok := parseHookEvent(data, now)
	if !ok {
		return
	}
	if f.capOff {
		ev.Cap, ev.Src = "", ""
		f.act.observe(ev, now)
		return
	}
	if f.capt == nil || ev.Event != HookPreToolUse || ev.Sidechain || !f.act.isGardener(ev.Agent) {
		f.act.observe(ev, now)
		return
	}
	if ev.capTool != "" {
		f.capt.Uncaptioned(ev.capTool)
	}
	if !hb.bash || ev.Cap != "" {
		f.capt.Count(ev.Src)
		f.act.observe(ev, now)
		return
	}
	// late may run before observe has numbered the start: it waits for the number.
	var start uint64
	ready := make(chan struct{})
	agent, key := ev.Agent, callKey(ev)
	caption, src, pending := f.capt.Caption(hb.command, func(c string) {
		<-ready
		f.act.lateCaption(agent, start, c, key, f.now())
	})
	if caption != "" {
		ev.Cap, ev.Src = caption, src
	}
	ev.haiku = pending // its transcript row waits for the caption (transcript.go)
	start = f.act.observe(ev, now)
	close(ready)
}

// ShareLineArms has the farm read its wishes' line arms (FarmWish.Arms) through h's line-arms cache, the one the ledger's
// rows are dressed from, so a bead both show is read once. Call it before Start.
func (f *Farm) ShareLineArms(h *Hub) {
	if h != nil && h.line != nil {
		f.line = h.line
	}
}

// SetCaptioner sets the Haiku fallback ObserveHook asks for a Bash call's caption, and counts every caption with. Call
// it before Start.
func (f *Farm) SetCaptioner(k *captioner.Captioner) { f.capt = k }

// SetCaptionSettings sets the captions' switch, the wire cap and the transcript ring's rows (CaptionSettingsFromEnv).
// A bound out of range keeps its default. Call it before Start.
func (f *Farm) SetCaptionSettings(c CaptionSettings) {
	f.capOff = c.Off
	f.act.mu.Lock()
	defer f.act.mu.Unlock()
	if c.WireCap >= 1 && c.WireCap <= wireCaptionMax {
		f.act.wireCap = c.WireCap
	}
	if c.TranscriptRows >= 1 && c.TranscriptRows <= maxTranscriptRows {
		f.act.ringRows = c.TranscriptRows
	}
}

// ObserveThinking takes a telepathy thought by the agent bead agent: think, unless a call or subagent is out. Nil-safe.
func (f *Farm) ObserveThinking(agent string) {
	if f == nil || agent == "" {
		return
	}
	now := f.now()
	f.act.observe(HookActivity{Agent: agent, Event: HookThinking, At: now}, now)
}

// ObserveSay takes what the agent bead agent said (an assistant text block coop forwarded, ThinkingHub.OnSay): a say
// event, a bird, and a row of its transcript at once (gd-HHGjxt59Mr). The text is scrubbed as a caption is (paths
// shortened, secrets out, one line) and clipped to sayMax runes; the captions switch does not drop it, since the text is
// the row's whole point, not a caption of it. Nil-safe; an empty text is nothing said.
func (f *Farm) ObserveSay(agent, text string) {
	if f == nil || agent == "" {
		return
	}
	said := scrubText(text, sayMax)
	if said == "" {
		return
	}
	now := f.now()
	f.act.observe(HookActivity{Agent: agent, Event: HookSay, Cap: said, At: now}, now)
}

// SubscribeActivity registers a viewer of the activity feed's events (activity, busy, doing). keep is the viewer's cut
// of gardeners (nil: every gardener). sid is the stream's id for SetActivityView. cancel closes the channel.
func (f *Farm) SubscribeActivity(keep func(FarmGardener) bool) (sid string, ch <-chan ActivityMsg, cancel func()) {
	return f.act.subscribe(keep)
}

// SetActivityView narrows the activity stream sid to what its page can see (gd-fBGGNdgrLc); false when there is no
// such stream.
func (f *Farm) SetActivityView(sid string, v ActivityView) bool {
	return f.act.setView(sid, v, f.now())
}

// ActivityStats is the activity feed's part of Stats, without building the snapshot: for a metrics scrape.
func (f *Farm) ActivityStats() FarmStats {
	f.act.mu.Lock()
	defer f.act.mu.Unlock()
	return FarmStats{ActivityClients: len(f.act.clients), ActivitySent: f.act.sent, ActivityBytes: f.act.bytes, ActivityDropped: f.act.dropped, ActivityViews: f.act.views,
		ActivityFoldClients: f.act.tiers, ActivityFoldBytes: f.act.tierBytes,
		TranscriptRows: f.act.rows, TranscriptClients: f.act.trClients, TranscriptBytes: f.act.trBytes}
}

func (f *Farm) loop(ctx context.Context) {
	defer close(f.done)
	reconcile := time.NewTicker(farmReconcile)
	defer reconcile.Stop()
	var lastEnd time.Time
	fails := 0 // failed refreshes in a row
	// settle waits out a burst of work events, then drains the one that came
	// in meanwhile; false means ctx ended.
	settle := func() bool {
		wait := farmSettle
		if gap := farmMinGap - time.Since(lastEnd); gap > wait {
			wait = gap
		}
		if !sleep(ctx, wait) {
			return false
		}
		select {
		case <-f.dirty:
		default:
		}
		return true
	}
	for {
		full := true
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			full = f.now().Sub(f.rowsAt) >= farmFullEvery
		case <-f.rosterDirty:
			full = false
			if gap := farmRosterGap - time.Since(lastEnd); gap > 0 {
				// A work event during the wait must not wait for the roster gap.
				t := time.NewTimer(gap)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				case <-f.dirty:
					t.Stop()
					full = true
					if !settle() {
						return
					}
				}
			}
		case <-f.dirty:
			if !settle() {
				return
			}
		}
		if full {
			// A full refresh re-reads the agents too.
			select {
			case <-f.rosterDirty:
			default:
			}
		}
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		var err error
		if full {
			err = f.refresh(rctx)
		} else {
			err = f.refreshRoster(rctx)
		}
		cancel()
		lastEnd = time.Now()
		if err == nil {
			fails = 0
			continue
		}
		fails++
		wait := farmRetryAfter(fails)
		f.logger.Warn("garden farm: refresh failed", "error", err, "failures", fails, "retry_in", wait)
		go func() {
			if sleep(ctx, wait) {
				f.Invalidate()
			}
		}()
	}
}

func (f *Farm) refresh(ctx context.Context) error       { return f.refreshTimed(ctx, true) }
func (f *Farm) refreshRoster(ctx context.Context) error { return f.refreshTimed(ctx, false) }

func (f *Farm) refreshTimed(ctx context.Context, readRows bool) error {
	start := time.Now()
	err := f.refreshOnce(ctx, readRows)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRefresh = time.Since(start)
	if err != nil {
		f.lastErr = err.Error()
	} else {
		f.lastErr = ""
	}
	return err
}
