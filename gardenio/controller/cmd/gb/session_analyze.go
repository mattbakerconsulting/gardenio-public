package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var sessionAnalyzeCmd = &cobra.Command{
	Use:   "analyze [file]",
	Short: "Analyze a session JSONL log for cost and error metrics",
	Long: `Analyze a Claude Code session JSONL log and display key metrics:
  - Total tokens (input/output/cache)
  - Estimated cost (USD)
  - Tool call count
  - Tool error count + error rate
  - Context compactions
  - Session duration

If no file is given, analyzes the most recent session log.
Supports S3 URIs (s3://bucket/key) if aws cli is available.

With --token-signals, also emit per-turn token arrays and derived
efficiency signals (cache-hit ratio, peak context, output-per-request,
redundant context spikes, ephemeral 1h/5m cache split). These are token
counts and token/token ratios only — never a USD figure computed from
tokens; authoritative cost stays with LiteLLM.

With --efficiency-cap, ALSO evaluate the CED Efficiency token-cost hard-cap
(Token-eff 3, ka-0000007291) over those signals and attach an efficiency_cap
verdict. The verdict is a pure function of the objective counters, so two
graders get identical results — it is the reproducible half of the CED judge
step. Implies --token-signals.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionAnalyze,
}

// sessionTokenSignals is the --token-signals flag: when set, runSessionAnalyze
// runs the extra per-turn pass and attaches a TokenSignals block to the result.
// This is the MEASURE half — purely objective, no scoring.
var sessionTokenSignals bool

// sessionEfficiencyCap is the --efficiency-cap flag: when set, runSessionAnalyze
// additionally evaluates the CED Efficiency token-cost hard-cap over the token
// signals and attaches an efficiency_cap verdict. This is the JUDGE half — the
// scoring decision. It implies --token-signals (the cap needs the signals).
var sessionEfficiencyCap bool

func init() {
	sessionCmd.AddCommand(sessionAnalyzeCmd)
	sessionAnalyzeCmd.Flags().BoolVar(&sessionTokenSignals, "token-signals", false,
		"emit per-turn token arrays + derived efficiency signals (cache-hit ratio, peak context, spikes)")
	sessionAnalyzeCmd.Flags().BoolVar(&sessionEfficiencyCap, "efficiency-cap", false,
		"evaluate the CED Efficiency token-cost hard-cap over the token signals (implies --token-signals)")
}

// sessionAnalyzeResult is the structured output for --json mode.
type sessionAnalyzeResult struct {
	File             string  `json:"file"`
	DurationSeconds  float64 `json:"duration_seconds"`
	Duration         string  `json:"duration"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	RequestCount     int64   `json:"request_count"`
	ToolCallCount    int64   `json:"tool_call_count"`
	ErrorCount       int64   `json:"error_count"`
	ErrorRate        float64 `json:"error_rate"`
	CompactionCount  int64   `json:"compaction_count"`
	// TokenSignals is populated only when --token-signals is passed; omitted
	// from JSON otherwise so the default output stays unchanged.
	TokenSignals *TokenSignals `json:"token_signals,omitempty"`
	// EfficiencyCap is populated only when --efficiency-cap is passed; it is the
	// CED judge-step verdict for the Efficiency token-cost sub-dimension. Omitted
	// otherwise so the default output stays unchanged.
	EfficiencyCap *EfficiencyCap `json:"efficiency_cap,omitempty"`
}

func runSessionAnalyze(cmd *cobra.Command, args []string) error {
	logPath, cleanup, err := resolveSessionLog(args)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	usage, err := parseSessionUsage(logPath)
	if err != nil {
		return fmt.Errorf("parsing session log: %w", err)
	}

	duration, err := extractSessionDuration(logPath)
	if err != nil {
		// Non-fatal: we can still report usage without duration.
		fmt.Fprintf(os.Stderr, "warning: could not extract session duration: %v\n", err)
	}

	result := buildAnalyzeResult(logPath, usage, duration)

	// --efficiency-cap implies --token-signals: the cap is evaluated over the
	// signals, so we always need them when the cap is requested.
	if sessionTokenSignals || sessionEfficiencyCap {
		signals, err := parseSessionTokenSignals(logPath)
		if err != nil {
			return fmt.Errorf("parsing token signals: %w", err)
		}
		result.TokenSignals = signals
	}

	if sessionEfficiencyCap {
		verdict := evaluateEfficiencyCap(result.TokenSignals)
		result.EfficiencyCap = &verdict
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	printAnalyzeTable(result)
	if result.TokenSignals != nil {
		printTokenSignals(result.TokenSignals)
	}
	if result.EfficiencyCap != nil {
		printEfficiencyCap(result.EfficiencyCap)
	}
	return nil
}

// resolveSessionLog determines the JSONL file path to analyze.
// Returns the local path, an optional cleanup function (for S3 temp files), and any error.
func resolveSessionLog(args []string) (string, func(), error) {
	if len(args) == 0 {
		path := findLatestSessionLog()
		if path == "" {
			return "", nil, fmt.Errorf("no session log found; pass a file path or S3 URI")
		}
		return path, nil, nil
	}

	path := args[0]
	if strings.HasPrefix(path, "s3://") {
		return downloadS3Log(path)
	}
	return path, nil, nil
}

// downloadS3Log downloads an S3 object to a temp file.
func downloadS3Log(s3URI string) (string, func(), error) {
	tmp, err := os.CreateTemp("", "gb-session-*.jsonl")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmp.Close()

	cmd := exec.Command("aws", "s3", "cp", s3URI, tmp.Name())
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("downloading %s: %w (is aws cli installed?)", s3URI, err)
	}

	cleanup := func() { os.Remove(tmp.Name()) }
	return tmp.Name(), cleanup, nil
}

// extractSessionDuration reads the first and last timestamps from a JSONL file.
func extractSessionDuration(logPath string) (time.Duration, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var firstTS, lastTS time.Time

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		ts := extractTimestamp(line)
		if ts.IsZero() {
			continue
		}

		if firstTS.IsZero() {
			firstTS = ts
		}
		lastTS = ts
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	if firstTS.IsZero() || lastTS.IsZero() {
		return 0, fmt.Errorf("no timestamps found in session log")
	}

	return lastTS.Sub(firstTS), nil
}

// extractTimestamp pulls the "timestamp" field from a JSONL line.
func extractTimestamp(line []byte) time.Time {
	var entry struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Timestamp == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
	return t
}

func buildAnalyzeResult(logPath string, usage *beadsapi.SessionUsage, duration time.Duration) sessionAnalyzeResult {
	var errorRate float64
	if usage.ToolCallCount > 0 {
		errorRate = float64(usage.ErrorCount) / float64(usage.ToolCallCount) * 100
	}

	return sessionAnalyzeResult{
		File:             logPath,
		DurationSeconds:  duration.Seconds(),
		Duration:         duration.Round(time.Second).String(),
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		TotalCostUSD:     usage.TotalCostUSD,
		RequestCount:     usage.RequestCount,
		ToolCallCount:    usage.ToolCallCount,
		ErrorCount:       usage.ErrorCount,
		ErrorRate:        errorRate,
		CompactionCount:  usage.CompactionCount,
	}
}

func printAnalyzeTable(r sessionAnalyzeResult) {
	fmt.Printf("Session: %s\n\n", r.File)
	fmt.Printf("  Duration:           %s\n", r.Duration)
	fmt.Printf("  API Requests:       %d\n", r.RequestCount)
	fmt.Println()
	fmt.Println("  Tokens:")
	fmt.Printf("    Input:            %d\n", r.InputTokens)
	fmt.Printf("    Output:           %d\n", r.OutputTokens)
	fmt.Printf("    Cache Read:       %d\n", r.CacheReadTokens)
	fmt.Printf("    Cache Write:      %d\n", r.CacheWriteTokens)
	fmt.Println()
	fmt.Printf("  Cost:               $%.2f\n", r.TotalCostUSD)
	fmt.Println()
	fmt.Printf("  Tool Calls:         %d\n", r.ToolCallCount)
	fmt.Printf("  Errors:             %d", r.ErrorCount)
	if r.ToolCallCount > 0 {
		fmt.Printf("  (%.1f%% error rate)", r.ErrorRate)
	}
	fmt.Println()
	fmt.Printf("  Compactions:        %d\n", r.CompactionCount)
}

// printTokenSignals renders the --token-signals block in human-readable form.
// It reports token counts and token/token ratios only — no USD is derived here.
func printTokenSignals(s *TokenSignals) {
	fmt.Println()
	fmt.Println("  Token signals:")
	fmt.Printf("    Turns:            %d\n", s.TurnCount)
	fmt.Printf("    Cache hit ratio:  %.3f\n", s.CacheHitRatio)
	fmt.Printf("    Output/request:   %.1f\n", s.OutputPerRequest)
	fmt.Printf("    Peak context:     %d\n", s.PeakContextTokens)
	fmt.Printf("    Redundant spikes: %d\n", s.RedundantContextSpikes)
	fmt.Printf("    Ephemeral 1h/5m:  %d / %d\n",
		s.EphemeralCacheCreation.Ephemeral1hInputTokens,
		s.EphemeralCacheCreation.Ephemeral5mInputTokens)
}

// printEfficiencyCap renders the CED Efficiency token-cost hard-cap verdict in
// human-readable form. It reports which objective conditions fired, the observed
// value vs the documented threshold, and the remediation tag (data-plane vs
// agent-behavior) — no USD, no token×rate.
func printEfficiencyCap(c *EfficiencyCap) {
	fmt.Println()
	fmt.Println("  Efficiency token-cost cap (CED judge):")
	if !c.Capped {
		fmt.Println("    Capped:           no (no token-cost condition fired)")
		return
	}
	fmt.Printf("    Capped:           yes — Efficiency dimension capped at %d/10\n", c.CapScore)
	for _, r := range c.Reasons {
		fmt.Printf("    - %-24s observed %.3f vs threshold %.3f  [%s]\n",
			r.Signal, r.Observed, r.Threshold, r.Tag)
	}
}
