package commons

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// batchCountingReader counts the channel reads by kind: one channel's
// (Fields channel_id) and a batch's (FieldsIn channel_id). With dropFieldIn
// it answers as a daemon older than field_in does: the whole land.
type batchCountingReader struct {
	Reader
	dropFieldIn   bool
	single, batch atomic.Int32
}

func (c *batchCountingReader) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	switch {
	case q.Fields[fieldChannelID] != "":
		c.single.Add(1)
	case len(q.FieldsIn[fieldChannelID]) > 0:
		c.batch.Add(1)
		if c.dropFieldIn {
			q.FieldsIn = nil
		}
	}
	return c.Reader.ListBeadsFiltered(ctx, q)
}

// manyDMs is TwoLands plus n DMs in gardenio, DM i holding two top-level
// messages ("dm i first", "dm i last") and a reply, and a viewer's read
// position on the first message of every even DM. With busy, the newest DM
// also holds more beads than a batch returns.
func manyDMs(n int, busy bool) (*commonstest.Daemon, map[string]string) {
	d := commonstest.TwoLands(t0)
	cursors := map[string]string{}
	msg := func(id, ch, thread, text string, at time.Time) {
		f := map[string]string{"channel_id": ch, "author": "agent:crew-" + ch, "text": text}
		if thread != "" {
			f["thread_id"] = thread
		}
		d.Put(&beadsapi.BeadDetail{ID: id, Type: "message", Title: text, Status: "closed", Labels: []string{"project:gardenio"}, CreatedAt: at, Fields: f})
	}
	for i := range n {
		ch := fmt.Sprintf("gd-dmx%03d", i)
		at := t0.Add(time.Duration(i) * time.Minute)
		d.Put(&beadsapi.BeadDetail{ID: ch, Type: "channel", Title: "dm " + ch, Status: "open", Labels: []string{"project:gardenio"}, CreatedAt: at,
			Fields: map[string]string{"name": "dm-" + ch, "channel_kind": "dm"}})
		msg(ch+"-a", ch, "", fmt.Sprintf("dm %d first", i), at.Add(10*time.Second))
		msg(ch+"-b", ch, "", fmt.Sprintf("dm %d last", i), at.Add(20*time.Second))
		msg(ch+"-r", ch, ch+"-a", "a reply", at.Add(30*time.Second))
		if i%2 == 0 {
			cursors[ch] = ch + "-a"
		}
	}
	if busy {
		ch := fmt.Sprintf("gd-dmx%03d", n-1)
		at := t0.Add(time.Duration(n) * time.Minute)
		for k := range batchLimit + 10 {
			msg(fmt.Sprintf("%s-busy%03d", ch, k), ch, ch+"-b", "busy reply", at.Add(time.Duration(k)*time.Second))
		}
	}
	return d, cursors
}

// readAll runs the counts on rd and returns each DM's badge, by channel id.
func readAll(t *testing.T, rd Reader, d *commonstest.Daemon, cursors map[string]string) map[string]Unread {
	t.Helper()
	lands, err := readLands(t.Context(), d, adminScope)
	if err != nil {
		t.Fatalf("readLands: %v", err)
	}
	return readUnread(t.Context(), rd, lands, cursors, "")
}

// TestReadChannels_DMsCostABatchedReadPerBatchChannels: the counts read a land's DMs batchChannels to a list call, not one call each
// (gd-OYKXtzN9kr: 330 DMs spent the 8 s budget), and answer them exactly as
// the per-channel reads do.
func TestReadChannels_DMsCostABatchedReadPerBatchChannels(t *testing.T) {
	const n = 2*batchChannels + 7
	d, cursors := manyDMs(n, false)
	rd := &batchCountingReader{Reader: d}
	unread := readAll(t, rd, d, cursors)

	small, smallCursors := manyDMs(3, false)
	srd := &batchCountingReader{Reader: small}
	readAll(t, srd, small, smallCursors)
	if got, want := rd.single.Load(), srd.single.Load(); got != want {
		t.Errorf("%d DMs sent %d per-channel reads, 3 DMs %d: per-channel reads grew with the DMs", n, got, want)
	}
	if got, want := rd.batch.Load(), int32(3); got != want { // three batches
		t.Errorf("%d DMs sent %d batched reads, want %d", n, got, want)
	}

	for i := range n {
		ch := fmt.Sprintf("gd-dmx%03d", i)
		want := 2
		if i%2 == 0 {
			want = 1
		}
		if u := unread[ch]; u.N != want || u.More {
			t.Errorf("%s unread = %+v, want %d", ch, u, want)
		}
	}

	// The same reads answered per channel (no batch answers anything when
	// every batch fails) agree with the batched ones.
	frd := &failBatchReader{Reader: d}
	funread := readAll(t, frd, d, cursors)
	for i := range n {
		ch := fmt.Sprintf("gd-dmx%03d", i)
		if funread[ch] != unread[ch] {
			t.Errorf("%s: per-channel %+v, batched %+v", ch, funread[ch], unread[ch])
		}
	}
}

// failBatchReader refuses every batched read, so readChannels falls back to
// one read per channel.
type failBatchReader struct{ Reader }

func (f *failBatchReader) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if len(q.FieldsIn) > 0 {
		return nil, &beadsapi.APIError{StatusCode: 503, Message: "no batches"}
	}
	return f.Reader.ListBeadsFiltered(ctx, q)
}

// TestReadChannels_ABatchCutShortStillAnswersRight: a DM busy enough to fill
// a batch's limit, and a daemon that ignores field_in and answers the whole
// land, leave some channels outside what the batch holds; those are read on
// their own, and every answer is still the per-channel one.
func TestReadChannels_ABatchCutShortStillAnswersRight(t *testing.T) {
	for _, tc := range []struct {
		name        string
		busy, oldKd bool
	}{{"busy DM", true, false}, {"daemon without field_in", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			d, cursors := manyDMs(batchChannels+5, tc.busy)
			wantU := readAll(t, &failBatchReader{Reader: d}, d, cursors)
			rd := &batchCountingReader{Reader: d, dropFieldIn: tc.oldKd}
			gotU := readAll(t, rd, d, cursors)
			for ch, w := range wantU {
				if gotU[ch] != w {
					t.Errorf("%s unread = %+v, want %+v", ch, gotU[ch], w)
				}
			}
			if len(gotU) != len(wantU) {
				t.Errorf("%d channels counted, want %d", len(gotU), len(wantU))
			}
			if tc.busy {
				quiet, qc := manyDMs(batchChannels+5, false)
				qrd := &batchCountingReader{Reader: quiet}
				readAll(t, qrd, quiet, qc)
				if rd.single.Load() <= qrd.single.Load() {
					t.Errorf("busy DM: %d per-channel reads, quiet %d: the cut-short batch answered every channel", rd.single.Load(), qrd.single.Load())
				}
			}
		})
	}
}
