package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// ── slack files ────────────────────────────────────────────────────

var slackFilesCmd = &cobra.Command{
	Use:   "files",
	Short: "List and download Slack file attachments from the current thread",
	Long: `Slack file commands surface attachments posted in the originating Slack
thread (the one this agent was spawned from) and download them through the
bridge proxy.

Typical agent flow for handling a screenshot:

  gb slack files fetch-all   # downloads to ./slack-files/<file_id>-<name>
  # then use the Read tool on the saved path

The bridge proxies Slack file downloads at GET /api/slack/files/{file_id},
streaming the file with the bot token. Files larger than 50MB are rejected.`,
}

var slackFilesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List file attachments in the current Slack thread",
	Long: `Prints one line per attachment (id, name, mimetype, size, proxy URL) for
every file posted in the originating Slack thread.`,
	RunE: runSlackFilesList,
}

var slackFilesFetchCmd = &cobra.Command{
	Use:   "fetch <file_id>",
	Short: "Download a single Slack file attachment by file_id",
	Long: `Streams the file behind the bridge proxy URL to disk. Default destination
is ./slack-files/<file_id>-<name>; override with -o/--output.

The file_id is the value shown next to each attachment by 'gb slack files
list' or in the [attachment: ...] hints surfaced by 'gb slack thread'.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackFilesFetch,
}

var slackFilesFetchAllCmd = &cobra.Command{
	Use:   "fetch-all",
	Short: "Download every file attachment in the current Slack thread",
	Long: `Lists files in the originating Slack thread and downloads each one to
./slack-files/<file_id>-<name> (override the directory with -d/--dir).
Files already present on disk are skipped unless --force is passed.`,
	RunE: runSlackFilesFetchAll,
}

func init() {
	slackFilesFetchCmd.Flags().StringP("output", "o", "", "destination path (default ./slack-files/<file_id>-<name>)")

	slackFilesFetchAllCmd.Flags().StringP("dir", "d", "slack-files", "directory to write files into")
	slackFilesFetchAllCmd.Flags().Bool("force", false, "overwrite existing files instead of skipping")
	slackFilesFetchAllCmd.Flags().Bool("thread", true, "fetch from the originating thread (the only supported mode today)")

	slackFilesListCmd.Flags().Bool("thread", true, "list files from the originating thread (the only supported mode today)")

	slackFilesCmd.AddCommand(slackFilesListCmd)
	slackFilesCmd.AddCommand(slackFilesFetchCmd)
	slackFilesCmd.AddCommand(slackFilesFetchAllCmd)

	slackCmd.AddCommand(slackFilesCmd)
}

// listThreadFiles fetches the current thread and returns every file attachment
// across every message. The returned slice preserves message order.
func listThreadFiles() ([]slackFileInfo, error) {
	bridgeURL, channel, threadTS, err := resolveSlackEnv()
	if err != nil {
		return nil, err
	}

	msgs := fetchThreadMessages(bridgeURL, channel, threadTS, 200)
	if msgs == nil {
		return nil, fmt.Errorf("failed to fetch thread messages from bridge")
	}

	var out []slackFileInfo
	for _, m := range msgs {
		out = append(out, m.Files...)
	}
	return out, nil
}

func runSlackFilesList(cmd *cobra.Command, args []string) error {
	files, err := listThreadFiles()
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(files)
		return nil
	}

	if len(files) == 0 {
		fmt.Println("(No attachments in this thread)")
		return nil
	}

	for _, f := range files {
		fmt.Printf("%s  %-30s  %-20s  %10s  %s\n",
			f.ID,
			truncate(f.Name, 30),
			truncate(f.Mimetype, 20),
			formatSize(int64(f.Size)),
			f.ProxyURL,
		)
	}
	return nil
}

// resolveFileProxyURL returns the absolute proxy URL for a file_id, given the
// bridge URL. The bridge serves attachments at /api/slack/files/{file_id}.
func resolveFileProxyURL(bridgeURL, fileID string) string {
	return strings.TrimRight(bridgeURL, "/") + "/api/slack/files/" + url.PathEscape(fileID)
}

// downloadProxyFile streams the bridge proxy URL for fileID to dest. It
// returns the final destination path and any download error. If dest is the
// empty string, the default `./slack-files/<file_id>-<name>` layout is used.
// When name is empty (e.g. only the file_id is known), only the file_id is
// used in the default name.
func downloadProxyFile(bridgeURL, fileID, name, dest string) (string, error) {
	if dest == "" {
		base := fileID
		if name != "" {
			base = fileID + "-" + sanitizeFilename(name)
		}
		dest = filepath.Join("slack-files", base)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return dest, fmt.Errorf("creating output directory: %w", err)
	}

	u := resolveFileProxyURL(bridgeURL, fileID)
	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return dest, fmt.Errorf("fetching %s: %w", fileID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return dest, fmt.Errorf("bridge returned HTTP %d for %s: %s", resp.StatusCode, fileID, strings.TrimSpace(string(body)))
	}

	// If the caller didn't supply a name, try the Content-Disposition.
	if name == "" {
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			if extracted := filenameFromContentDisposition(cd); extracted != "" {
				dest = filepath.Join(filepath.Dir(dest), fileID+"-"+sanitizeFilename(extracted))
			}
		}
	}

	f, err := os.Create(dest)
	if err != nil {
		return dest, fmt.Errorf("creating %s: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(dest)
		return dest, fmt.Errorf("writing %s: %w", dest, err)
	}
	return dest, nil
}

// filenameFromContentDisposition pulls the filename out of an
// `attachment; filename="..."` header. Best-effort, returns "" on miss.
func filenameFromContentDisposition(cd string) string {
	for part := range strings.SplitSeq(cd, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(strings.ToLower(part), "filename=") {
			continue
		}
		v := strings.TrimPrefix(part, "filename=")
		v = strings.TrimPrefix(v, "filename=")
		v = strings.Trim(v, `"`)
		return v
	}
	return ""
}

// sanitizeFilename strips path separators and other shell-hostile characters.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "/" {
		return "file"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', '\x00':
			return '_'
		}
		return r
	}, name)
}

func runSlackFilesFetch(cmd *cobra.Command, args []string) error {
	fileID := args[0]
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	dest, _ := cmd.Flags().GetString("output")

	// Try to get a friendly name from the thread when available so the default
	// path uses <file_id>-<name>. Failure is non-fatal — the bridge response
	// will still tell us the filename.
	name := ""
	if dest == "" {
		if files, err := listThreadFiles(); err == nil {
			for _, f := range files {
				if f.ID == fileID {
					name = f.Name
					break
				}
			}
		}
	}

	final, err := downloadProxyFile(bridgeURL, fileID, name, dest)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(map[string]string{"file_id": fileID, "path": final})
		return nil
	}
	fmt.Printf("Downloaded %s -> %s\n", fileID, final)
	return nil
}

func runSlackFilesFetchAll(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	files, err := listThreadFiles()
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("(No attachments in this thread)")
		return nil
	}

	dir, _ := cmd.Flags().GetString("dir")
	force, _ := cmd.Flags().GetBool("force")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	type result struct {
		FileID  string `json:"file_id"`
		Path    string `json:"path"`
		Skipped bool   `json:"skipped,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	var results []result

	for _, f := range files {
		dest := filepath.Join(dir, f.ID+"-"+sanitizeFilename(f.Name))
		if !force {
			if _, statErr := os.Stat(dest); statErr == nil {
				results = append(results, result{FileID: f.ID, Path: dest, Skipped: true})
				if !jsonOutput {
					fmt.Printf("Skipped %s (already at %s)\n", f.ID, dest)
				}
				continue
			}
		}
		final, err := downloadProxyFile(bridgeURL, f.ID, f.Name, dest)
		if err != nil {
			results = append(results, result{FileID: f.ID, Path: final, Error: err.Error()})
			if !jsonOutput {
				fmt.Printf("FAILED %s: %v\n", f.ID, err)
			}
			continue
		}
		results = append(results, result{FileID: f.ID, Path: final})
		if !jsonOutput {
			fmt.Printf("Downloaded %s -> %s\n", f.ID, final)
		}
	}

	if jsonOutput {
		printJSON(results)
	}
	return nil
}

