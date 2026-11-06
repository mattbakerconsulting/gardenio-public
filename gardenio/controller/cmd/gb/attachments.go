package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// isDarkPod reports whether this gb process runs inside a dark agent pod, i.e.
// the controller stamped GARDENIO_DARK=true (beadsapi.EnvGardenioDark). Dark pods
// get a READ-ONLY Jira client so any write fails closed — the client-layer leg
// of the dark-Jira write-token fence (ka-0000005242).
func isDarkPod() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(beadsapi.EnvGardenioDark)), "true")
}

// attJiraKeyRe matches a strict JIRA key (e.g., "PE-7001").
var attJiraKeyRe = regexp.MustCompile(`^[A-Z]+-\d+$`)

var (
	attJiraURL   string
	attJiraEmail string
	attJiraToken string
)

var attachmentsCmd = &cobra.Command{
	Use:     "attachments",
	Short:   "JIRA attachment commands",
	GroupID: "orchestration",
}

var attachmentsListCmd = &cobra.Command{
	Use:   "list <bead-id-or-jira-key>",
	Short: "List JIRA attachments for a bead or issue key",
	Args:  cobra.ExactArgs(1),
	RunE:  runAttachmentsList,
}

var attachmentsDownloadCmd = &cobra.Command{
	Use:   "download <bead-id-or-jira-key> [filename-glob]",
	Short: "Download JIRA attachments to a local directory",
	Long: `Download JIRA attachments for a bead or issue key.

If a filename glob is provided, only matching attachments are downloaded.
By default all attachments are downloaded.

Examples:
  gb attachments download PE-7001
  gb attachments download PE-7001 "*.png" -o /tmp
  gb attachments download my-bead-id --all`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runAttachmentsDownload,
}

var attachmentsUploadCmd = &cobra.Command{
	Use:   "upload <bead-id-or-jira-key> <file>...",
	Short: "Attach local files to a JIRA issue so they render inline",
	Long: `Attach local files (screenshots, videos) to a JIRA issue.

WHY THIS EXISTS. 'gb image upload' stores bytes in a PRIVATE bucket and prints a
viewer-proxied URL that only resolves through the viewer's OIDC chain. Pasting
that URL into a JIRA ticket renders a BROKEN IMAGE: JIRA's renderer fetches it
anonymously and gets bounced. The only way to embed an image in JIRA is to
attach the bytes to the issue and reference the returned attachment id from an
ADF media node.

This command prints one 'attachment:<id>' reference per file. Feed those to a
body written in markdown as the image destination:

  ![before](attachment:10501)

...and the renderer turns it into an inline image instead of a dead link.

Examples:
  gb attachments upload GBFSM-42 before.png after.png
  gb attachments upload my-bead-id proof.png --json`,
	Args: cobra.MinimumNArgs(2),
	RunE: runAttachmentsUpload,
}

func init() {
	attachmentsCmd.PersistentFlags().StringVar(&attJiraURL, "jira-url", os.Getenv("JIRA_BASE_URL"), "JIRA base URL")
	attachmentsCmd.PersistentFlags().StringVar(&attJiraEmail, "jira-email", os.Getenv("JIRA_EMAIL"), "JIRA email")
	attachmentsCmd.PersistentFlags().StringVar(&attJiraToken, "jira-token", os.Getenv("JIRA_API_TOKEN"), "JIRA API token")

	attachmentsDownloadCmd.Flags().StringP("output-dir", "o", ".", "Directory to save downloaded files")
	attachmentsDownloadCmd.Flags().Bool("all", false, "Download all attachments without listing first")

	attachmentsUploadCmd.Flags().Bool("json", false, "emit the attachment records as JSON")

	attachmentsCmd.AddCommand(attachmentsListCmd)
	attachmentsCmd.AddCommand(attachmentsDownloadCmd)
	attachmentsCmd.AddCommand(attachmentsUploadCmd)
}

func runAttachmentsUpload(cmd *cobra.Command, args []string) error {
	jiraKey, err := resolveJiraKey(cmd, args[0])
	if err != nil {
		return err
	}
	jiraClient, err := newAttJiraClient()
	if err != nil {
		return err
	}

	asJSON, _ := cmd.Flags().GetBool("json")
	uploaded := make([]*bridge.JiraAttachmentUpload, 0, len(args)-1)

	for _, path := range args[1:] {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		att, err := jiraClient.AddAttachment(cmd.Context(), jiraKey, filepath.Base(path), data)
		if err != nil {
			return fmt.Errorf("attaching %s to %s: %w", path, jiraKey, err)
		}
		uploaded = append(uploaded, att)
		if !asJSON {
			// The attachment: reference is the point — it is what a markdown
			// body uses as an image destination to get an inline render.
			fmt.Printf("attachment:%s\t%s\n", att.ID, att.Filename)
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(uploaded)
	}
	fmt.Fprintf(os.Stderr, "Attached %d file(s) to %s. Reference them in a markdown body as ![alt](attachment:<id>).\n",
		len(uploaded), jiraKey)
	return nil
}

// resolveJiraKey resolves a bead ID or JIRA key argument to a JIRA issue key.
func resolveJiraKey(cmd *cobra.Command, arg string) (string, error) {
	if attJiraKeyRe.MatchString(arg) {
		return arg, nil
	}
	bead, err := daemon.GetBead(cmd.Context(), arg)
	if err != nil {
		return "", fmt.Errorf("fetching bead %s: %w", arg, err)
	}
	jiraKey := bead.Fields["jira_key"]
	if jiraKey == "" {
		return "", fmt.Errorf("bead %s has no jira_key field", arg)
	}
	return jiraKey, nil
}

// newAttJiraClient constructs a JiraClient from the shared attachment flags.
// In a dark agent pod (GARDENIO_DARK=true) the client is READ-ONLY, so any write
// (e.g. gb attachments upload) fails closed with ErrJiraReadOnly rather than
// touching real upstream Jira — the client-layer leg of the dark-Jira
// write-token fence (ka-0000005242).
func newAttJiraClient() (*bridge.JiraClient, error) {
	if attJiraURL == "" {
		return nil, fmt.Errorf("--jira-url or JIRA_BASE_URL is required")
	}
	return bridge.NewJiraClient(bridge.JiraClientConfig{
		BaseURL:  attJiraURL,
		Email:    attJiraEmail,
		APIToken: attJiraToken,
		ReadOnly: isDarkPod(),
	}), nil
}

func runAttachmentsList(cmd *cobra.Command, args []string) error {
	jiraKey, err := resolveJiraKey(cmd, args[0])
	if err != nil {
		return err
	}

	jiraClient, err := newAttJiraClient()
	if err != nil {
		return err
	}

	issue, err := jiraClient.GetIssue(cmd.Context(), jiraKey)
	if err != nil {
		return fmt.Errorf("fetching JIRA issue %s: %w", jiraKey, err)
	}

	attachments := issue.Fields.Attachments
	if len(attachments) == 0 {
		if jsonOutput {
			printJSON([]any{})
		} else {
			cmd.Printf("No attachments for %s\n", jiraKey)
		}
		return nil
	}

	if jsonOutput {
		data, err := json.MarshalIndent(attachments, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	// Print table.
	cmd.Printf("%-30s %-20s %10s  %s\n", "FILENAME", "MIME TYPE", "SIZE", "URL")
	cmd.Printf("%-30s %-20s %10s  %s\n",
		strings.Repeat("-", 30), strings.Repeat("-", 20), strings.Repeat("-", 10), strings.Repeat("-", 40))
	for _, att := range attachments {
		cmd.Printf("%-30s %-20s %10s  %s\n",
			truncate(att.Filename, 30),
			truncate(att.MimeType, 20),
			formatSize(att.Size),
			att.Content)
	}
	return nil
}

func runAttachmentsDownload(cmd *cobra.Command, args []string) error {
	jiraKey, err := resolveJiraKey(cmd, args[0])
	if err != nil {
		return err
	}

	jiraClient, err := newAttJiraClient()
	if err != nil {
		return err
	}

	outputDir, _ := cmd.Flags().GetString("output-dir")

	// Parse optional filename glob filter.
	var globPattern string
	if len(args) > 1 {
		globPattern = args[1]
	}

	issue, err := jiraClient.GetIssue(cmd.Context(), jiraKey)
	if err != nil {
		return fmt.Errorf("fetching JIRA issue %s: %w", jiraKey, err)
	}

	attachments := issue.Fields.Attachments
	if len(attachments) == 0 {
		cmd.Printf("No attachments for %s\n", jiraKey)
		return nil
	}

	// Filter by glob if provided.
	if globPattern != "" {
		var filtered []bridge.JiraAttachment
		for _, att := range attachments {
			matched, err := filepath.Match(globPattern, att.Filename)
			if err != nil {
				return fmt.Errorf("invalid glob pattern %q: %w", globPattern, err)
			}
			if matched {
				filtered = append(filtered, att)
			}
		}
		if len(filtered) == 0 {
			cmd.Printf("No attachments matching %q for %s\n", globPattern, jiraKey)
			return nil
		}
		attachments = filtered
	}

	// Ensure output directory exists.
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Print what we're downloading.
	cmd.Printf("Downloading %d attachment(s) from %s to %s\n", len(attachments), jiraKey, outputDir)

	for _, att := range attachments {
		destPath := filepath.Join(outputDir, att.Filename)

		f, err := os.Create(destPath)
		if err != nil {
			return fmt.Errorf("creating %s: %w", att.Filename, err)
		}

		if err := jiraClient.DownloadAttachment(cmd.Context(), att.Content, f); err != nil {
			f.Close()
			os.Remove(destPath)
			return fmt.Errorf("downloading %s: %w", att.Filename, err)
		}
		f.Close()

		cmd.Printf("  Downloaded %s (%s)\n", att.Filename, formatSize(att.Size))
	}

	return nil
}

func formatSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
