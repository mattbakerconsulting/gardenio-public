package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// gb say is the one verb an agent speaks with (design spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md, section
// 4). Where a message shows up is routing, not the agent's problem: the bridge
// writes the message bead and projects it to Slack and the Commons.
//
// gb squawk, gb slack reply, gb slack upload and gb slack thread are permanent
// aliases (owner decision gd-FuxIp9SMtV): a plain `gb say "x"` is a squawk,
// `gb slack reply` is `gb say` into the home thread, `gb slack upload` is
// `gb say --file`, and `gb slack thread` is `gb thread`.

var (
	sayThread  string
	sayChannel string
	sayFiles   []string
	sayTitle   string
	sayAltText string
	sayTo      string
	sayAsk     bool
	sayTask    string
	sayWake    bool
)

var sayCmd = &cobra.Command{
	Use:   "say [message]",
	Short: "Say something in your thread, another thread, or a channel of your land",
	Long: `Say something where people (and agents) will see it. You never name a
surface: the bridge records the message as a bead and shows it in Slack and
the Commons, wherever the thread or channel lives.

  gb say "text"                          your home thread (same as gb squawk)
  gb say --thread <head-id> "text"       a reply in that thread
  gb say --channel general "text"        a new post in a channel of your land
  gb say --file shot.png ["text"]        attach files (repeat --file); goes to
                                         --thread / --channel, else your home thread
  gb say --to ada@example.com "text"     a DM to a person or an agent
  gb say --to ada@example.com --ask "q?" a DM question: the reply is captured
                                         and you are nudged with it
  gb say --thread <head-id> --wake "text"
                                         a reply that also wakes the thread's
                                         agent with it

A plain 'gb say' is a squawk: a progress line in your home thread, prefixed
with your name. With --thread, --channel or --file the text is posted as you
wrote it, and the bridge answers with the message bead's id.

--thread takes a thread head's bead id (or the id of any message in the
thread). --channel takes a channel name (#general, crew), a channel bead id or
the Slack channel id it is bound to; you can only post in your own land.

--wake gives the reply to the thread's agent too, as @gardenio in the thread
would: a nudge when it runs, a respawn with the thread's conversation when it
has stopped, a fresh agent when the thread has none. An agent's say is never a
mention, so "@gardenio" in the text wakes no one; --wake is how an agent (the
herder, on a wish it judged unfulfilled) wakes a thread's agent. The woken
agent runs as the person who asked in the thread. An identical say inside the
dedup window is not posted again and wakes no one.

--to takes an email (or oidc:<email>), a Slack user id, a GitLab or GitHub
username, or agent:<name> (an agent of your land). The DM is a channel of your
land in the Commons, made on first use; a person also gets it as a Slack DM,
as gb dm send has always sent it. With --ask the answer comes back to you as
a nudge, whether it is a reply in the Slack DM's thread or a message in the
Commons DM; read the whole exchange with gb thread <id>.

Aliases, kept for good: gb squawk = gb say; gb slack reply = gb say into your
home thread; gb slack upload = gb say --file. Read a thread with gb thread.`,
	GroupID: "orchestration",
	Args:    cobra.MaximumNArgs(1),
	RunE:    runSay,
}

func init() {
	sayCmd.Flags().StringVar(&sayThread, "thread", "", "reply in this thread (thread head bead id)")
	sayCmd.Flags().StringVar(&sayChannel, "channel", "", "post in this channel of your land (name, channel bead id, or bound Slack channel id)")
	sayCmd.Flags().StringArrayVar(&sayFiles, "file", nil, "attach a file (repeatable); posts to --thread/--channel or your home thread")
	sayCmd.Flags().StringVar(&sayTitle, "title", "", "title of the attached file (one --file only; defaults to its name)")
	sayCmd.Flags().StringVar(&sayAltText, "alt-text", "", "accessibility alt text for the attached image (one --file only)")
	sayCmd.Flags().BoolVar(&squawkNoComment, "no-comment", false,
		"Do not auto-comment the message on the actor's currently-claimed bead")
	sayCmd.Flags().BoolVar(&squawkToMR, "to-mr", false,
		"Plain say only: also mirror it to the MR-bound agent's GitLab MR as a comment")
	sayCmd.Flags().StringVar(&sayTo, "to", "", "DM this person (email, Slack user id, GitLab/GitHub username) or agent (agent:<name>)")
	sayCmd.Flags().BoolVar(&sayAsk, "ask", false, "with --to: ask a question; the reply is captured and nudges you")
	sayCmd.Flags().StringVar(&sayTask, "task", "", "with --ask: the task bead it is about (default: $BOAT_TASK_ID)")
	sayCmd.Flags().BoolVar(&sayWake, "wake", false, "with --thread: also give the words to the thread's agent, waking it when it has stopped")
	sayCmd.MarkFlagsMutuallyExclusive("thread", "channel", "to")
	sayCmd.MarkFlagsMutuallyExclusive("file", "to")
}

func runSay(cmd *cobra.Command, args []string) error {
	text := ""
	if len(args) == 1 {
		text = args[0]
	}
	if sayAsk && sayTo == "" {
		return fmt.Errorf("--ask needs --to: the person or agent you are asking")
	}
	if sayTask != "" && !sayAsk {
		return fmt.Errorf("--task applies to --ask")
	}
	if sayWake && sayThread == "" {
		return fmt.Errorf("--wake needs --thread: the head of the thread whose agent it wakes")
	}
	if sayThread == "" && sayChannel == "" && len(sayFiles) == 0 && sayTo == "" {
		if len(args) == 0 {
			return fmt.Errorf("say what? pass the message text")
		}
		return runSquawk(cmd, args)
	}
	if squawkToMR {
		return fmt.Errorf("--to-mr applies to a plain gb say (a squawk), not to --thread/--channel/--file")
	}
	if strings.TrimSpace(text) == "" && len(sayFiles) == 0 {
		return fmt.Errorf("message text cannot be empty")
	}
	if len(sayFiles) > 1 && (sayTitle != "" || sayAltText != "") {
		return fmt.Errorf("--title and --alt-text describe one file; pass a single --file")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the bridge")
	}
	req := newSayParams(text)
	req.Thread, req.Channel, req.Wake = sayThread, sayChannel, sayWake
	if sayTo != "" {
		req.To, req.Ask, req.Task = sayTo, sayAsk, sayTask
		if req.Ask && req.Task == "" {
			req.Task = os.Getenv("BOAT_TASK_ID")
		}
	} else if req.Thread == "" && req.Channel == "" {
		if err := req.toHomeThread(ctx); err != nil {
			return err
		}
	}
	files, err := readSayFiles(sayFiles, defaultMaxSlackUploadBytes)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		files[0].title, files[0].altText = sayTitle, sayAltText
	}

	result, err := conversationSay(ctx, bridgeURL, req, files)
	if req.To != "" && (errors.Is(err, errBridgeWithoutConversation) || (err == nil && result.DM == "")) {
		// A bridge that predates --to has no route, or read the say as one
		// with no target.
		return fmt.Errorf("the bridge at %s predates gb say --to; use gb dm send / gb dm ask", bridgeURL)
	}
	if errors.Is(err, errBridgeWithoutConversation) {
		return fmt.Errorf("the bridge at %s predates gb say --thread/--channel/--file; use gb slack reply / gb slack upload", bridgeURL)
	}
	if err != nil {
		return err
	}
	if !squawkNoComment && result.ID != "" && daemon != nil {
		autoCommentClaimedBead(ctx, result.ID, text)
	}
	if jsonOutput {
		printJSON(result)
		return nil
	}
	switch {
	case result.Deduped:
		fmt.Println("OK (identical message already posted)")
	case result.ID != "" && req.Ask:
		fmt.Printf("OK %s\nRead the answer with: gb thread %s\n", result.ID, result.ID)
	case result.ID != "":
		fmt.Printf("OK %s\n", result.ID)
	default:
		fmt.Println("OK")
	}
	if result.Woke != "" {
		fmt.Println(result.Woke)
	} else if req.Wake && !result.Deduped {
		// A bridge that predates --wake posts the reply and ignores it.
		fmt.Fprintln(os.Stderr, "warning: the bridge did not report a wake; it may predate gb say --wake")
	}
	return nil
}

// sayParams is the bridge's say request (bridge.sayRequest).
type sayParams struct {
	Agent       string `json:"agent"`
	AgentBead   string `json:"agent_bead,omitempty"`
	TriggerBead string `json:"trigger_bead,omitempty"`
	Land        string `json:"land,omitempty"`
	Thread      string `json:"thread,omitempty"`
	Channel     string `json:"channel,omitempty"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	Text        string `json:"text"`
	To          string `json:"to,omitempty"`
	Ask         bool   `json:"ask,omitempty"`
	Task        string `json:"task,omitempty"`
	Wake        bool   `json:"wake,omitempty"`
}

// newSayParams is a say of text by this agent.
func newSayParams(text string) sayParams {
	agentBead := os.Getenv("BOAT_AGENT_BEAD_ID")
	if agentBead == "" {
		agentBead = os.Getenv("KD_AGENT_ID")
	}
	land := os.Getenv("BOAT_PROJECT")
	if land == "" {
		land = os.Getenv("KD_PROJECT")
	}
	return sayParams{
		Agent:       actor,
		AgentBead:   agentBead,
		TriggerBead: os.Getenv("BOAT_TRIGGER_BEAD_ID"),
		Land:        land,
		Text:        text,
	}
}

// toHomeThread targets the agent's home thread: its Slack thread coordinates
// (env, else the agent bead, as gb slack reply has always resolved them), else
// the thread head on its agent bead (thread_id).
func (p *sayParams) toHomeThread(ctx context.Context) error {
	if channel, ts := resolveThreadCoords(ctx); channel != "" && ts != "" {
		p.Channel, p.ThreadTS = channel, ts
		return nil
	}
	if head := homeThreadHead(ctx); head != "" {
		p.Thread = head
		return nil
	}
	return fmt.Errorf("no home thread: SLACK_THREAD_CHANNEL/SLACK_THREAD_TS are not set and the agent bead names no thread; pass --thread or --channel")
}

// homeThreadHead returns the thread head on the agent bead's thread_id.
func homeThreadHead(ctx context.Context) string {
	beadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if beadID == "" {
		beadID = os.Getenv("KD_AGENT_ID")
	}
	if beadID == "" || daemon == nil {
		return ""
	}
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		return ""
	}
	return bead.Fields["thread_id"]
}

// sayFile is a file to attach to a say.
type sayFile struct {
	name    string
	data    []byte
	title   string
	altText string
}

// readSayFiles reads each path, refusing directories, empty files and files
// over maxBytes.
func readSayFiles(paths []string, maxBytes int64) ([]sayFile, error) {
	var files []sayFile
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory; pass a file", path)
		}
		if info.Size() == 0 {
			return nil, fmt.Errorf("%s is empty", path)
		}
		if info.Size() > maxBytes {
			return nil, fmt.Errorf("file %s is %d bytes (>%d)", path, info.Size(), maxBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		files = append(files, sayFile{name: filepath.Base(path), data: data})
	}
	return files, nil
}

// sayResult is the bridge's say response (bridge.sayResponse).
type sayResult struct {
	OK        bool   `json:"ok"`
	ID        string `json:"id,omitempty"`
	Thread    string `json:"thread,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Deduped   bool   `json:"deduped,omitempty"`
	DM        string `json:"dm,omitempty"`
	Woke      string `json:"woke,omitempty"`
}

// errBridgeWithoutConversation means the bridge has no /api/conversation
// routes (it predates them, or runs without a Slack bot); the aliases fall
// back to the /api/slack/threads routes.
var errBridgeWithoutConversation = errors.New("bridge has no /api/conversation routes")

// conversationUnavailable reports whether a response means the route is not
// there rather than that the say was refused: a 404/405 from the mux, or the
// plain-text 401/403 bridgeRBAC gives a route it does not know (the agent
// bearer is only valid on agent routes). The conversation routes answer every
// refusal of their own as JSON.
func conversationUnavailable(resp *http.Response, body []byte) bool {
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnauthorized, http.StatusForbidden:
		return !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{"))
	}
	return false
}

// bridgeError turns a non-200 bridge response into an error, with the
// bridge's JSON error message when it sent one.
func bridgeError(resp *http.Response, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, e.Error)
	}
	return fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// conversationSay posts a say to the bridge: JSON for text, multipart when
// files are attached.
func conversationSay(ctx context.Context, bridgeURL string, p sayParams, files []sayFile) (*sayResult, error) {
	if agent, _ := bridgeAgentIdentity(ctx); agent != "" {
		p.Agent = agent
	}
	var body bytes.Buffer
	contentType := "application/json"
	if len(files) == 0 {
		if err := json.NewEncoder(&body).Encode(p); err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
	} else {
		mw := multipart.NewWriter(&body)
		fields := [][2]string{
			{"agent", p.Agent}, {"agent_bead", p.AgentBead}, {"trigger_bead", p.TriggerBead}, {"land", p.Land},
			{"thread", p.Thread}, {"channel", p.Channel}, {"thread_ts", p.ThreadTS},
			{"text", p.Text}, {"title", files[0].title}, {"alt_txt", files[0].altText},
		}
		if p.Wake {
			fields = append(fields, [2]string{"wake", "true"})
		}
		for _, f := range fields {
			if f[1] == "" {
				continue
			}
			if err := mw.WriteField(f[0], f[1]); err != nil {
				return nil, fmt.Errorf("building request: %w", err)
			}
		}
		for _, f := range files {
			part, err := mw.CreateFormFile("file", f.name)
			if err != nil {
				return nil, fmt.Errorf("building request: %w", err)
			}
			if _, err := part.Write(f.data); err != nil {
				return nil, fmt.Errorf("building request: %w", err)
			}
		}
		if err := mw.Close(); err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		contentType = mw.FormDataContentType()
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/conversation/say"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	setBridgeAgentToken(ctx, req)
	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting to bridge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := readLimited(resp.Body, 4096)
		if conversationUnavailable(resp, msg) {
			return nil, errBridgeWithoutConversation
		}
		return nil, bridgeError(resp, msg)
	}
	var result sayResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}
