package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestHandleUnreleasedCommand_NoGitHub(t *testing.T) {
	daemon := newMockDaemon()

	// Capture the ephemeral message posted.
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			_ = r.ParseForm()
			postedText = r.FormValue("text")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// github is nil by default in newTestBot.

	bot.handleUnreleasedCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})

	if postedText == "" {
		t.Fatal("expected ephemeral message to be posted")
	}
	if postedText != Garden.Semantic(SemFail)+" No git provider configured (GITLAB_API_TOKEN or GITHUB_TOKEN not set)" {
		t.Errorf("unexpected message: %s", postedText)
	}
}

func TestHandleUnreleasedCommand_WithRepos(t *testing.T) {
	daemon := newMockDaemon()

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/myrepo/tags":
			writeJSON(w, []ghTag{{Name: "2026.60.1"}})
		case "/repos/org/myrepo/compare/2026.60.1...main":
			writeJSON(w, ghCompare{
				AheadBy: 1,
				Commits: []struct {
					SHA    string `json:"sha"`
					Commit struct {
						Message string `json:"message"`
						Author  struct {
							Name string `json:"name"`
						} `json:"author"`
					} `json:"commit"`
				}{
					{SHA: "abc1234567890", Commit: struct {
						Message string `json:"message"`
						Author  struct {
							Name string `json:"name"`
						} `json:"author"`
					}{Message: "fix: something", Author: struct {
						Name string `json:"name"`
					}{Name: "Dev"}}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	var ephemeralPosted bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			ephemeralPosted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.github = newTestGitHubClient(ghSrv.URL, "")
	bot.repos = []RepoRef{{Owner: "org", Repo: "myrepo"}}
	bot.version = "test-v1"

	bot.handleUnreleasedCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})

	if !ephemeralPosted {
		t.Error("expected ephemeral message to be posted")
	}
}

func TestHandleUnreleasedCommand_UpToDate(t *testing.T) {
	daemon := newMockDaemon()

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/tags":
			writeJSON(w, []ghTag{{Name: "2026.61.1"}})
		case "/repos/org/repo/compare/2026.61.1...main":
			writeJSON(w, ghCompare{AheadBy: 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	var ephemeralPosted bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			ephemeralPosted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.github = newTestGitHubClient(ghSrv.URL, "")
	bot.repos = []RepoRef{{Owner: "org", Repo: "repo"}}
	bot.version = "v1"

	bot.handleUnreleasedCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})

	if !ephemeralPosted {
		t.Error("expected ephemeral message to be posted")
	}
}

// TestHandleUnreleasedCommand_ImageFilesTruncated is the user-visible proof for
// ka-0000004642: when GitLab caps the compare diffs array (compare_timeout=true),
// the /unreleased digest must render the image file count as a lower bound
// ("N+") and say the count was capped, instead of a silently-truncated exact
// number. It captures the Block Kit payload posted to Slack and asserts on the
// header text.
func TestHandleUnreleasedCommand_ImageFilesTruncated(t *testing.T) {
	daemon := newMockDaemon()

	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/projects/Acme/Platform/gardenio/repository/compare" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"commits": [{"id": "aaa111", "author_name": "Alice"}],
				"diffs": [
					{"new_path": "gardenio/images/agent/entrypoint.sh"},
					{"new_path": "gardenio/images/agent/install/versions.env"}
				],
				"compare_timeout": true,
				"compare_same_ref": false
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer glSrv.Close()

	var postedBlocks string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			_ = r.ParseForm()
			postedBlocks = r.FormValue("blocks")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.gitlab = newTestGitLabClient(glSrv.URL)
	bot.version = "test-v1"
	bot.imageConfigs = []ImageTrackConfig{
		{
			Name:        "agent",
			ProjectPath: "Acme/Platform/gardenio",
			Tag:         "2026.106.3",
			Paths:       []string{"gardenio/images/agent/"},
		},
	}

	bot.handleUnreleasedCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})

	if postedBlocks == "" {
		t.Fatal("expected Block Kit payload to be posted")
	}
	// Lower-bound marker on the count.
	if !strings.Contains(postedBlocks, "*2+* files changed") {
		t.Errorf("expected header to render count as a lower bound (\"*2+* files changed\"); blocks:\n%s", postedBlocks)
	}
	// Explicit truncation notice. Slack-go escapes "(" -> "(", so match on
	// the stable substring rather than the literal parenthesized phrase.
	if !strings.Contains(postedBlocks, "GitLab capped the diff at 3000 files") {
		t.Errorf("expected a truncation notice in the header; blocks:\n%s", postedBlocks)
	}
}

func TestHandleUnreleasedCommand_WithControllerVersion(t *testing.T) {
	daemon := newMockDaemon()

	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			writeJSON(w, controllerVersionInfo{
				Version:    "v3.0.0",
				Commit:     "deadbeef12345678",
				AgentImage: "agent:latest",
				Namespace:  "gardenio",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrlSrv.Close()

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ghSrv.Close()

	var ephemeralPosted bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			ephemeralPosted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.github = newTestGitHubClient(ghSrv.URL, "")
	bot.controllerURL = ctrlSrv.URL
	bot.version = "v1"

	bot.handleUnreleasedCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})

	if !ephemeralPosted {
		t.Error("expected ephemeral message to be posted")
	}
}
