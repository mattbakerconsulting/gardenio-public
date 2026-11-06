package deathtranscript

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/codex/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseEvents_CodexFixtures(t *testing.T) {
	turns := ParseEvents(readFixture(t, "hook_events.jsonl"), readFixture(t, "state_events.jsonl"))

	var got []string
	for _, tr := range turns {
		got = append(got, tr.Type+": "+tr.Text)
	}
	// Interleaved by wall clock; the repeated final message collapses; the
	// garbage line, SessionStart and PreToolUse contribute nothing.
	want := []string{
		"user: run the tool",
		"tool: [tool: Bash] echo hello-from-tool",
		"assistant: first done",
		"tool: [tool: Bash] curl -H 'Authorization: Bearer [REDACTED]' https://example.com/x",
		"assistant: all finished, pushed the branch",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d turns %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if i == 3 {
			// Redaction marker text is toollatency's; assert on the secret instead.
			if strings.Contains(got[i], "sk-abcdefghijklmnop") {
				t.Errorf("turn 3 leaked a credential: %q", got[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("turn %d = %q, want %q", i, got[i], want[i])
		}
	}
	if turns[1].Tool != "Bash" || turns[1].Command != "echo hello-from-tool" {
		t.Errorf("tool turn = %+v", turns[1])
	}
	if turns[0].Timestamp.IsZero() || !turns[0].Timestamp.Before(turns[1].Timestamp) {
		t.Errorf("timestamps not ordered: %v %v", turns[0].Timestamp, turns[1].Timestamp)
	}
}

func TestParseEvents_Empty(t *testing.T) {
	if got := ParseEvents(nil, nil); len(got) != 0 {
		t.Fatalf("ParseEvents(nil,nil) = %v", got)
	}
}

func TestToolCommand(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"string command", `{"command":"ls -la"}`, "ls -la"},
		{"argv command", `{"command":["bash","-lc","ls"]}`, "bash -lc ls"},
		{"no command key", `{"path": "a.go"}`, `{"path":"a.go"}`},
		{"empty", ``, ""},
		{"null", `null`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolCommand([]byte(tc.in)); got != tc.want {
				t.Errorf("toolCommand(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLastCommand(t *testing.T) {
	turns := ParseEvents(readFixture(t, "hook_events.jsonl"), nil)
	if got := LastCommand(turns); !strings.HasPrefix(got, "curl ") {
		t.Errorf("LastCommand = %q", got)
	}
	if got := LastCommand(nil); got != "" {
		t.Errorf("LastCommand(nil) = %q", got)
	}
}

func TestIsEventStreamHarness(t *testing.T) {
	for h, want := range map[string]bool{"codex": true, " codex ": true, "claude": false, "": false, "gemini": true, " gemini ": true, "other": false} {
		if got := IsEventStreamHarness(h); got != want {
			t.Errorf("IsEventStreamHarness(%q) = %v, want %v", h, got, want)
		}
	}
}

func codexStore(t *testing.T) *mockStore {
	t.Helper()
	st := newMockStore()
	base := "coop/sessions/" + beadID + "/"
	st.put(base+spawnA+"/meta.json", `{"started_at": 100, "harness": "claude"}`)
	st.put(base+spawnB+"/meta.json", `{"started_at": 200, "harness": "codex"}`)
	st.put(base+spawnA+"/hook_events.jsonl", `{"json":{"event":"post_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"old"}}},"timestamp_ms":1}`)
	st.put(base+spawnB+"/hook_events.jsonl", string(readFixture(t, "hook_events.jsonl")))
	st.put(base+spawnB+"/state_events.jsonl", string(readFixture(t, "state_events.jsonl")))
	st.put(base+spawnB+"/session.jsonl", `{"type":"session_meta"}`)
	return st
}

func TestFetchLatestEvents_PicksNewestSpawnAndHarness(t *testing.T) {
	f := NewFetcher(codexStore(t), "")
	ev, err := f.FetchLatestEvents(context.Background(), beadID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Spawn != spawnB || ev.Harness != "codex" {
		t.Errorf("spawn=%q harness=%q", ev.Spawn, ev.Harness)
	}
	if !strings.HasSuffix(ev.Key, spawnB+"/hook_events.jsonl") {
		t.Errorf("Key = %q", ev.Key)
	}
	if len(ev.Hook) == 0 || len(ev.State) == 0 {
		t.Errorf("hook=%d state=%d bytes", len(ev.Hook), len(ev.State))
	}
	if h := f.SessionHarness(context.Background(), beadID); h != "codex" {
		t.Errorf("SessionHarness = %q", h)
	}
}

func TestFetchLatestEvents_StateOnlyAndFlatLayout(t *testing.T) {
	st := newMockStore()
	st.put("coop/sessions/"+beadID+"/state_events.jsonl", `{"last_message":"bye","timestamp_ms":5}`)
	ev, err := NewFetcher(st, "").FetchLatestEvents(context.Background(), beadID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Spawn != "" || ev.Hook != nil || !strings.HasSuffix(ev.Key, "/state_events.jsonl") {
		t.Errorf("got %+v", ev)
	}
}

func TestFetchLatestEvents_None(t *testing.T) {
	st := newMockStore()
	st.put("coop/sessions/"+beadID+"/"+spawnA+"/transcripts/1.jsonl", `{}`)
	_, err := NewFetcher(st, "").FetchLatestEvents(context.Background(), beadID)
	if !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("err = %v, want ErrNoTranscript", err)
	}
}

func TestSessionHarness_FallsBackToTranscriptSpawn(t *testing.T) {
	st := newMockStore()
	st.put("coop/sessions/"+beadID+"/"+spawnA+"/transcripts/1.jsonl", `{}`)
	st.put("coop/sessions/"+beadID+"/"+spawnA+"/meta.json", `{"started_at":1,"harness":"claude"}`)
	if h := NewFetcher(st, "").SessionHarness(context.Background(), beadID); h != "claude" {
		t.Errorf("SessionHarness = %q", h)
	}
	if h := NewFetcher(newMockStore(), "").SessionHarness(context.Background(), beadID); h != "" {
		t.Errorf("empty store SessionHarness = %q", h)
	}
}

// The Claude path is untouched: transcripts/ chunks never look like event logs.
func TestEventObjectsIgnoresTranscriptsAndWorkflow(t *testing.T) {
	st := newMockStore()
	base := "coop/sessions/" + beadID + "/" + spawnA + "/"
	st.put(base+"transcripts/1.jsonl", `{}`)
	st.put(base+"workflow/hook_events.jsonl", `{}`)
	spawns, _ := NewFetcher(st, "").eventObjects(context.Background(), beadID)
	if len(spawns) != 0 {
		t.Fatalf("eventObjects = %v, want none", spawns)
	}
}
