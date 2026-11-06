package gardenhub

import "time"

// fixtureTranscript gives the card world's gardener id a transcript (bird-captions spec 11.4; the transcript golden and
// commons-ux-acceptance surface): 60 calls, a minute apart up to now, of every class a bird flies for, with captions short
// and long (F1 a 200-rune one, F2 a URL, a SHA and a path; no F4, whose glyphs no pinned font draws), one call that failed
// and one let go unfinished. Its rows go straight into the ring, as finished calls would leave them: the fixture farm hears
// no hooks.
func fixtureTranscript(f *Farm, id string, now time.Time) {
	caps := []struct{ class, tool, cap string }{
		{"shell", "go_test", "Run the gardenhub transcript tests"},
		{"read", "Read", "Read gardenio/controller/internal/gardenhub/web/transcript.js"},
		{"edit", "Edit", "Edit web/garden.css"},
		{"git", "git_push", "Push the branch agent/transcript-view/gd-kl2CL3X9jQ"},
		{"beads", "kd_show", "Show gd-kl2CL3X9jQ"},
		{"shell", "other", ""}, // no caption: the tool word
		{"web", "WebFetch", "Fetch https://github.com/mattbakerconsulting/gardenio/pull/1467/files#diff-3f2a9c1e0b7d4a6e8f5c2b1a0d9e8f7c6b5a4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"},
		{"delegate", "subagent", "Explore how the farm stream hands events over"},
		{"shell", "other", "Trial-merge main into #1282, then rebuild the page bundle and check that every embedded script still fits the budget we agreed in costs_frames_test.go, and say which file grew the most since 44ece186 if it does not fit"},
		{"test", "go_test", "Run go test -run TestTranscript ./internal/gardenhub/"},
		{"build", "go_build", "Build beads-viewer"},
		{"deploy", "helm", "Diff the chart against the cluster"},
	}
	f.act.mu.Lock()
	defer f.act.mu.Unlock()
	st := f.act.agents[id]
	if st == nil {
		st = &actState{open: map[string]openCall{}, subs: map[string]time.Time{}}
		f.act.agents[id] = st
	}
	for i := 59; i >= 0; i-- {
		c := caps[i%len(caps)]
		row := CallRow{At: now.Add(-time.Duration(i)*time.Minute - 7*time.Second).Unix(), Class: c.class, Tool: c.tool, Cap: c.cap, Dur: int64(300 + 997*(i%9)), OK: outcome(i != 4)}
		if i == 2 {
			row.Dur, row.OK = 0, nil // let go unfinished
		}
		st.ring.add(row, transcriptRows)
	}
}

// fixturePlotCalls gives the gardeners of one plot interleaved transcripts (spec 11.5, gd-X5dM2VRgDd; the transcriptplot
// golden): twelve calls each, 40 s and 80 s apart, so runs of one gardener's rows and a switch between them both show, one failed.
func fixturePlotCalls(f *Farm, ids []string, now time.Time) {
	caps := []struct{ class, tool, cap string }{
		{"read", "Read", "Read web/transcript.js"}, {"shell", "other", "Trial-merge main into the plot branch"},
		{"test", "go_test", "Run go test -run TestTranscriptFarmPanel"}, {"edit", "Edit", "Edit web/garden.css"},
		{"git", "git_push", "Push agent/farm-plot-transcript"}, {"beads", "kd_show", "Show the plan"},
	}
	f.act.mu.Lock()
	defer f.act.mu.Unlock()
	for j, id := range ids {
		st := f.act.agents[id]
		if st == nil {
			st = &actState{open: map[string]openCall{}, subs: map[string]time.Time{}}
			f.act.agents[id] = st
		}
		for i := 11; i >= 0; i-- {
			c := caps[(i+j)%len(caps)]
			at := now.Add(-time.Duration(i*40*(j+1)+j*13) * time.Second).Unix() // g-ask-b twice as often; never one second for both
			st.ring.add(CallRow{At: at, Class: c.class, Tool: c.tool, Cap: c.cap, Dur: int64(400 + 731*((i+j)%7)), OK: outcome(i != 5 || j != 1)}, transcriptRows)
		}
	}
}
