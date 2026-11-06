package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	peekPlain      bool
	peekANSI       bool
	peekStatus     bool
	peekOutput     bool
	peekTail       int64
	peekOffset     int64
	peekLimit      int64
	peekTranscript string
	peekRecording  bool
	peekWatch      bool
	peekFollow     bool
	peekInterval   float64
)

var peekCmd = &cobra.Command{
	Use:   "peek [target]",
	Short: "View agent terminal sessions via coopmux",
	Long: `View agent terminal sessions through the coopmux multiplexer.

Without arguments, lists all registered sessions.
With a target, shows the current terminal screen for that session.

Target can be a session UUID, partial session ID, pod name, or agent name.

Requires COOP_MUX_URL to be set (and optionally COOP_MUX_TOKEN for auth).`,
	GroupID: "orchestration",
	Args:    cobra.MaximumNArgs(1),
	RunE:    runPeek,
}

func init() {
	peekCmd.Flags().BoolVar(&peekPlain, "plain", false, "plain text output (no ANSI escape codes)")
	peekCmd.Flags().BoolVar(&peekANSI, "ansi", false, "force ANSI-colored output")
	peekCmd.Flags().BoolVar(&peekStatus, "status", false, "show session status details")
	peekCmd.Flags().BoolVar(&peekOutput, "output", false, "show raw PTY output from ring buffer")
	peekCmd.Flags().Int64Var(&peekTail, "tail", 0, "show last N bytes of raw output")
	peekCmd.Flags().Int64Var(&peekOffset, "offset", -1, "start output from byte offset")
	peekCmd.Flags().Int64Var(&peekLimit, "limit", 0, "limit output to N bytes")
	peekCmd.Flags().StringVar(&peekTranscript, "transcripts", "", "list transcripts or fetch by number (list, latest, or N)")
	peekCmd.Flags().BoolVar(&peekRecording, "recording", false, "show recording status and entries")
	peekCmd.Flags().BoolVar(&peekWatch, "watch", false, "continuously refresh screen display")
	peekCmd.Flags().BoolVar(&peekFollow, "follow", false, "follow raw output stream (like tail -f)")
	peekCmd.Flags().Float64Var(&peekInterval, "interval", 0, "poll interval in seconds (default: 1 for --watch, 0.5 for --follow)")
	peekCmd.MarkFlagsMutuallyExclusive("plain", "ansi")
}

func runPeek(cmd *cobra.Command, args []string) error {
	muxURL, token, err := resolveMuxEnv()
	if err != nil {
		return err
	}

	client := muxHTTPClient()

	if len(args) == 0 {
		return peekListSessions(client, muxURL, token)
	}

	target := args[0]
	switch {
	case peekWatch:
		return peekWatchScreen(client, muxURL, token, target)
	case peekFollow:
		return peekFollowOutput(client, muxURL, token, target)
	case peekStatus:
		return peekShowStatus(client, muxURL, token, target)
	case peekOutput || peekTail > 0:
		return peekShowOutput(client, muxURL, token, target)
	case cmd.Flags().Changed("transcripts"):
		return peekShowTranscripts(client, muxURL, token, target)
	case peekRecording:
		return peekShowRecording(client, muxURL, token, target)
	default:
		return peekShowScreen(client, muxURL, token, target)
	}
}

// resolveMuxEnv reads and validates the required coopmux env vars.
func resolveMuxEnv() (muxURL, token string, err error) {
	muxURL = os.Getenv("COOP_MUX_URL")
	if muxURL == "" {
		return "", "", fmt.Errorf("COOP_MUX_URL is not set — cannot reach coopmux")
	}
	muxURL = strings.TrimRight(muxURL, "/")
	token = os.Getenv("COOP_MUX_TOKEN")
	return muxURL, token, nil
}

// muxHTTPClient returns an HTTP client with a reasonable timeout.
func muxHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// muxGet performs an authenticated GET request to the mux.
func muxGet(client *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

// peekListSessions lists all registered coopmux sessions.
func peekListSessions(client *http.Client, muxURL, token string) error {
	resp, err := muxGet(client, muxURL+"/api/v1/sessions", token)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var sessions []peekSessionInfo
	if err := json.Unmarshal(body, &sessions); err != nil {
		// If we can't parse as structured data, dump raw response.
		fmt.Println(string(body))
		return nil
	}

	if jsonOutput {
		printJSON(sessions)
		return nil
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions registered.")
		return nil
	}

	fmt.Printf("%-38s %-30s %-12s %s\n", "SESSION ID", "POD", "STATE", "HEALTH")
	fmt.Println(strings.Repeat("-", 88))
	for _, s := range sessions {
		health := "ok"
		if s.HealthFailures > 0 {
			health = fmt.Sprintf("%d failures", s.HealthFailures)
		}
		fmt.Printf("%-38s %-30s %-12s %s\n", s.ID, s.Pod(), s.State(), health)
	}
	fmt.Printf("\n%d session(s)\n", len(sessions))

	return nil
}

// peekShowScreen shows the current terminal screen for a session.
func peekShowScreen(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	resp, err := muxGet(client, muxURL+"/api/v1/sessions/"+sessionID+"/screen", token)
	if err != nil {
		return fmt.Errorf("fetching screen: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var screen peekScreenSnapshot
	if err := json.Unmarshal(body, &screen); err != nil {
		fmt.Println(string(body))
		return nil
	}

	if jsonOutput {
		printJSON(screen)
		return nil
	}

	// Choose ANSI or plain output.
	useANSI := peekANSI || (!peekPlain && isTerminal())
	lines := screen.Lines
	if useANSI && len(screen.ANSI) > 0 {
		lines = screen.ANSI
	}

	for _, line := range lines {
		fmt.Println(line)
	}

	// Show dimensions on stderr so they don't pollute piped output.
	if screen.Cols > 0 && screen.Rows > 0 {
		fmt.Fprintf(os.Stderr, "[%dx%d]\n", screen.Cols, screen.Rows)
	}

	return nil
}

// resolveSessionTarget resolves a target (session ID, partial ID, pod name, or agent name)
// to a full session UUID.
func resolveSessionTarget(client *http.Client, muxURL, token, target string) (string, error) {
	// If it looks like a full UUID, use it directly.
	if len(target) == 36 && strings.Count(target, "-") == 4 {
		return target, nil
	}

	// List sessions and try to match.
	resp, err := muxGet(client, muxURL+"/api/v1/sessions", token)
	if err != nil {
		return "", fmt.Errorf("listing sessions for resolution: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var sessions []peekSessionInfo
	if err := json.Unmarshal(body, &sessions); err != nil {
		return "", fmt.Errorf("parsing sessions: %w", err)
	}

	targetLower := strings.ToLower(target)
	var matches []peekSessionInfo

	for _, s := range sessions {
		if strings.HasPrefix(strings.ToLower(s.ID), targetLower) {
			matches = append(matches, s)
		} else if strings.Contains(strings.ToLower(s.Pod()), targetLower) {
			matches = append(matches, s)
		}
	}

	// If no direct match, try agent name resolution via beads daemon.
	if len(matches) == 0 && daemon != nil {
		if podName := resolveAgentPodName(target); podName != "" {
			podLower := strings.ToLower(podName)
			for _, s := range sessions {
				if strings.Contains(strings.ToLower(s.Pod()), podLower) {
					matches = append(matches, s)
				}
			}
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session matching '%s'", target)
	case 1:
		return matches[0].ID, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "'%s' matches %d sessions:\n", target, len(matches))
		for _, m := range matches {
			fmt.Fprintf(&b, "  %s  (%s)\n", m.ID, m.Pod())
		}
		return "", fmt.Errorf("%s", b.String())
	}
}

// isTerminal returns true if stdout is a terminal.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// peekShowStatus shows the status of a session.
func peekShowStatus(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	resp, err := muxGet(client, muxURL+"/api/v1/sessions/"+sessionID+"/status", token)
	if err != nil {
		return fmt.Errorf("fetching status: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var status peekSessionStatus
	if err := json.Unmarshal(body, &status); err != nil {
		fmt.Println(string(body))
		return nil
	}

	if jsonOutput {
		printJSON(status)
		return nil
	}

	fmt.Printf("Session:       %s\n", status.SessionID)
	fmt.Printf("State:         %s\n", status.State)
	if status.PID != nil {
		fmt.Printf("PID:           %d\n", *status.PID)
	}
	fmt.Printf("Uptime:        %s\n", peekFormatDuration(status.UptimeSecs))
	if status.ExitCode != nil {
		fmt.Printf("Exit Code:     %d\n", *status.ExitCode)
	}
	fmt.Printf("Screen Seq:    %d\n", status.ScreenSeq)
	fmt.Printf("Bytes Read:    %s\n", formatBytes(status.BytesRead))
	fmt.Printf("Bytes Written: %s\n", formatBytes(status.BytesWritten))
	fmt.Printf("WS Clients:    %d\n", status.WSClients)

	return nil
}

// peekFormatDuration formats seconds into a human-readable duration.
func peekFormatDuration(secs int64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm%ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%dh%dm%ds", secs/3600, (secs%3600)/60, secs%60)
}

// formatBytes formats a byte count into a human-readable string.
func formatBytes(b uint64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	}
}

// peekShowOutput dumps raw PTY output from the ring buffer.
func peekShowOutput(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	// Build query params.
	params := make([]string, 0, 3)
	if peekTail > 0 {
		// For --tail, first get total_written to calculate offset.
		infoResp, err := muxGet(client, muxURL+"/api/v1/sessions/"+sessionID+"/output", token)
		if err != nil {
			return fmt.Errorf("fetching output info: %w", err)
		}
		defer infoResp.Body.Close()
		infoBody, _ := io.ReadAll(infoResp.Body)
		var info peekOutputResponse
		if err := json.Unmarshal(infoBody, &info); err == nil && info.TotalWritten > uint64(peekTail) {
			params = append(params, "offset="+strconv.FormatUint(info.TotalWritten-uint64(peekTail), 10))
		}
	} else if peekOffset >= 0 {
		params = append(params, "offset="+strconv.FormatInt(peekOffset, 10))
	}
	if peekLimit > 0 {
		params = append(params, "limit="+strconv.FormatInt(peekLimit, 10))
	}

	url := muxURL + "/api/v1/sessions/" + sessionID + "/output"
	if len(params) > 0 {
		url += "?" + strings.Join(params, "&")
	}

	resp, err := muxGet(client, url, token)
	if err != nil {
		return fmt.Errorf("fetching output: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var output peekOutputResponse
	if err := json.Unmarshal(body, &output); err != nil {
		fmt.Println(string(body))
		return nil
	}

	if jsonOutput {
		printJSON(output)
		return nil
	}

	// Decode base64 data and write raw bytes to stdout.
	decoded, err := base64.StdEncoding.DecodeString(output.Data)
	if err != nil {
		return fmt.Errorf("decoding output data: %w", err)
	}
	os.Stdout.Write(decoded)

	// Show metadata on stderr.
	fmt.Fprintf(os.Stderr, "[offset=%d next=%d total=%d (%s)]\n",
		output.Offset, output.NextOffset, output.TotalWritten,
		formatBytes(output.TotalWritten))

	return nil
}

// peekShowTranscripts lists or fetches transcript snapshots.
func peekShowTranscripts(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	baseURL := muxURL + "/api/v1/sessions/" + sessionID + "/transcripts"

	switch peekTranscript {
	case "", "list":
		// List all transcripts.
		resp, err := muxGet(client, baseURL, token)
		if err != nil {
			return fmt.Errorf("listing transcripts: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
		}
		if jsonOutput {
			fmt.Println(string(body))
			return nil
		}
		var transcripts []peekTranscriptMeta
		if err := json.Unmarshal(body, &transcripts); err != nil {
			fmt.Println(string(body))
			return nil
		}
		if len(transcripts) == 0 {
			fmt.Println("No transcripts available.")
			return nil
		}
		fmt.Printf("%-8s %-24s %-10s %s\n", "NUMBER", "TIMESTAMP", "LINES", "SIZE")
		fmt.Println(strings.Repeat("-", 60))
		for _, t := range transcripts {
			fmt.Printf("%-8d %-24s %-10d %s\n", t.Number, t.Timestamp, t.LineCount, formatBytes(t.ByteSize))
		}
		fmt.Printf("\n%d transcript(s)\n", len(transcripts))
	default:
		// Fetch specific transcript by number.
		resp, err := muxGet(client, baseURL+"/"+peekTranscript, token)
		if err != nil {
			return fmt.Errorf("fetching transcript: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
		}
		fmt.Println(string(body))
	}
	return nil
}

// peekShowRecording shows recording status and entries.
func peekShowRecording(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	resp, err := muxGet(client, muxURL+"/api/v1/sessions/"+sessionID+"/recording", token)
	if err != nil {
		return fmt.Errorf("fetching recording: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coopmux returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	if jsonOutput {
		fmt.Println(string(body))
		return nil
	}

	var status peekRecordingStatus
	if err := json.Unmarshal(body, &status); err != nil {
		fmt.Println(string(body))
		return nil
	}

	fmt.Printf("Recording:  %v\n", status.Enabled)
	if status.Path != nil {
		fmt.Printf("Path:       %s\n", *status.Path)
	}
	fmt.Printf("Entries:    %d\n", status.Entries)

	return nil
}

// peekWatchScreen continuously refreshes the terminal screen display.
func peekWatchScreen(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	interval := time.Second
	if peekInterval > 0 {
		interval = time.Duration(peekInterval * float64(time.Second))
	}

	// Handle Ctrl+C gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	url := muxURL + "/api/v1/sessions/" + sessionID + "/screen"
	for {
		resp, err := muxGet(client, url, token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				var screen peekScreenSnapshot
				if err := json.Unmarshal(body, &screen); err == nil {
					// Clear screen and move cursor to top-left.
					fmt.Print("\033[2J\033[H")

					useANSI := peekANSI || (!peekPlain && isTerminal())
					lines := screen.Lines
					if useANSI && len(screen.ANSI) > 0 {
						lines = screen.ANSI
					}
					for _, line := range lines {
						fmt.Println(line)
					}
					fmt.Fprintf(os.Stderr, "[%dx%d seq=%d %s]\n",
						screen.Cols, screen.Rows, screen.Seq, time.Now().Format("15:04:05"))
				}
			}
		}

		select {
		case <-sigCh:
			return nil
		case <-time.After(interval):
		}
	}
}

// peekFollowOutput follows raw output stream incrementally (like tail -f).
func peekFollowOutput(client *http.Client, muxURL, token, target string) error {
	sessionID, err := resolveSessionTarget(client, muxURL, token, target)
	if err != nil {
		return err
	}

	interval := 500 * time.Millisecond
	if peekInterval > 0 {
		interval = time.Duration(peekInterval * float64(time.Second))
	}

	// Handle Ctrl+C gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	// Start from the end of current output.
	baseURL := muxURL + "/api/v1/sessions/" + sessionID + "/output"
	var offset uint64

	// Get initial total_written to start from the end.
	resp, err := muxGet(client, baseURL, token)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var info peekOutputResponse
		if json.Unmarshal(body, &info) == nil {
			offset = info.TotalWritten
		}
	}

	for {
		url := baseURL + "?offset=" + strconv.FormatUint(offset, 10)
		resp, err := muxGet(client, url, token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				var output peekOutputResponse
				if json.Unmarshal(body, &output) == nil && output.NextOffset > offset {
					decoded, err := base64.StdEncoding.DecodeString(output.Data)
					if err == nil && len(decoded) > 0 {
						os.Stdout.Write(decoded)
					}
					offset = output.NextOffset
				}
			}
		}

		select {
		case <-sigCh:
			return nil
		case <-time.After(interval):
		}
	}
}

// resolveAgentPodName looks up an agent bead by name and returns its pod name.
// Returns empty string if the agent is not found or has no pod.
func resolveAgentPodName(agentName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agents, err := daemon.ListAgentBeads(ctx)
	if err != nil {
		return ""
	}

	nameLower := strings.ToLower(agentName)
	for _, a := range agents {
		if strings.ToLower(a.AgentName) == nameLower || strings.ToLower(a.Title) == nameLower {
			if pod := a.Metadata["pod_name"]; pod != "" {
				return pod
			}
		}
	}
	return ""
}

// ── types ───────────────────────────────────────────────────────────

type peekSessionInfo struct {
	ID             string          `json:"id"`
	URL            string          `json:"url"`
	Metadata       json.RawMessage `json:"metadata"`
	RegisteredAtMS uint64          `json:"registered_at_ms"`
	HealthFailures uint32          `json:"health_failures"`
	CachedState    *string         `json:"cached_state,omitempty"`
}

// Pod extracts the pod name from session metadata.
func (s *peekSessionInfo) Pod() string {
	var meta struct {
		K8s struct {
			Pod string `json:"pod"`
		} `json:"k8s"`
	}
	if err := json.Unmarshal(s.Metadata, &meta); err == nil && meta.K8s.Pod != "" {
		return meta.K8s.Pod
	}
	return "-"
}

// State returns the cached state or "unknown".
func (s *peekSessionInfo) State() string {
	if s.CachedState != nil {
		return *s.CachedState
	}
	return "unknown"
}

type peekScreenSnapshot struct {
	Lines     []string `json:"lines"`
	ANSI      []string `json:"ansi"`
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	AltScreen bool     `json:"alt_screen"`
	Seq       uint64   `json:"seq"`
}

type peekSessionStatus struct {
	SessionID    string `json:"session_id"`
	State        string `json:"state"`
	PID          *int32 `json:"pid,omitempty"`
	UptimeSecs   int64  `json:"uptime_secs"`
	ExitCode     *int32 `json:"exit_code,omitempty"`
	ScreenSeq    uint64 `json:"screen_seq"`
	BytesRead    uint64 `json:"bytes_read"`
	BytesWritten uint64 `json:"bytes_written"`
	WSClients    int32  `json:"ws_clients"`
	FetchedAt    uint64 `json:"fetched_at"`
}

type peekOutputResponse struct {
	Data         string `json:"data"`
	Offset       uint64 `json:"offset"`
	NextOffset   uint64 `json:"next_offset"`
	TotalWritten uint64 `json:"total_written"`
}

type peekTranscriptMeta struct {
	Number    uint32 `json:"number"`
	Timestamp string `json:"timestamp"`
	LineCount uint64 `json:"line_count"`
	ByteSize  uint64 `json:"byte_size"`
}

type peekRecordingStatus struct {
	Enabled bool    `json:"enabled"`
	Path    *string `json:"path,omitempty"`
	Entries uint64  `json:"entries"`
}
