package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

const (
	gardenioMarkerStart = "<!-- gardenio:start — DO NOT EDIT BELOW THIS LINE -->"
	gardenioMarkerEnd   = "<!-- gardenio:end -->"
)

// MRDescriptionSection represents the context section that the bridge manages
// inside a GitLab MR description.
type MRDescriptionSection struct {
	BeadID         string
	JIRAKey        string
	JIRAStatus     string
	PipelineStatus string
	PipelineURL    string
	Approved       string
	Approvers      string
	MRState        string

	// ViewerBaseURL is the public beads-viewer base (e.g.
	// "https://gardenio.e2e.example.com/beads"). When set, the Bead line is
	// rendered as a Markdown deep-link into beads-viewer so reviewers can click
	// through from the MR. When empty, the bead ID falls back to plain text —
	// configs without a public viewer keep working unchanged. See
	// docs/design/bead-linking-in-gitlab.md (Gap 1a).
	ViewerBaseURL string

	// JIRABaseURL is the JIRA Cloud base (e.g.
	// "https://acme.atlassian.net"). When set, the JIRA line is rendered as
	// a Markdown deep-link to the issue. Empty falls back to plain text.
	JIRABaseURL string
}

// mdBeadRef renders the bead reference for the managed block: a Markdown
// link into beads-viewer when a base URL is configured, otherwise the bare ID.
// The viewer serves the bead-detail page at {base}/{id} ("GET /{id}",
// handleBeadShow); the "/b/{id}/..." routes are HTMX fragments only with no
// top-level page, so "{base}/b/{id}" 404s (ka-0000000801). Match every other
// bridge emitter (beadLink, model_fallback, refusal_log).
func mdBeadRef(beadID, viewerBase string) string {
	if beadID == "" {
		return ""
	}
	if viewerBase == "" {
		return beadID
	}
	return fmt.Sprintf("[%s](%s/%s)", beadID, strings.TrimRight(viewerBase, "/"), beadID)
}

// mdJiraRef renders the JIRA key as a Markdown link to the issue when a JIRA
// base URL is configured, otherwise the bare key.
func mdJiraRef(key, jiraBase string) string {
	if key == "" {
		return ""
	}
	if jiraBase == "" {
		return key
	}
	return fmt.Sprintf("[%s](%s/browse/%s)", key, strings.TrimRight(jiraBase, "/"), key)
}

// buildGardenioSection renders the managed section content (without markers).
func buildGardenioSection(s MRDescriptionSection) string {
	var b strings.Builder
	b.WriteString("### Agent Context\n")
	if s.BeadID != "" {
		b.WriteString(fmt.Sprintf("- **Bead:** %s\n", mdBeadRef(s.BeadID, s.ViewerBaseURL)))
	}
	if s.JIRAKey != "" {
		line := fmt.Sprintf("- **JIRA:** %s", mdJiraRef(s.JIRAKey, s.JIRABaseURL))
		if s.JIRAStatus != "" {
			line += fmt.Sprintf(" (%s)", s.JIRAStatus)
		}
		b.WriteString(line + "\n")
	}
	if s.MRState != "" {
		b.WriteString(fmt.Sprintf("- **MR State:** %s\n", s.MRState))
	}
	if s.PipelineStatus != "" {
		icon := pipelineIcon(s.PipelineStatus)
		line := fmt.Sprintf("- **Pipeline:** %s %s", icon, s.PipelineStatus)
		if s.PipelineURL != "" {
			line = fmt.Sprintf("- **Pipeline:** [%s %s](%s)", icon, s.PipelineStatus, s.PipelineURL)
		}
		b.WriteString(line + "\n")
	}
	if s.Approved != "" {
		approvedLine := fmt.Sprintf("- **Approved:** %s", s.Approved)
		if s.Approvers != "" {
			approvedLine += fmt.Sprintf(" (by %s)", s.Approvers)
		}
		b.WriteString(approvedLine + "\n")
	}
	return b.String()
}

// pipelineIcon returns a status icon for the pipeline status.
func pipelineIcon(status string) string {
	switch status {
	case "success":
		return "✅"
	case "failed":
		return "❌"
	case "running":
		return "🔄"
	case "pending", "created":
		return "⏳"
	case "canceled":
		return "🚫"
	default:
		return "⚙️"
	}
}

// spliceGardenioSection inserts or replaces the gardenio-managed section in an
// MR description. Content outside the markers is never modified.
func spliceGardenioSection(description, sectionContent string) string {
	wrapped := gardenioMarkerStart + "\n" + sectionContent + gardenioMarkerEnd

	startIdx := strings.Index(description, gardenioMarkerStart)
	endIdx := strings.Index(description, gardenioMarkerEnd)

	if startIdx >= 0 && endIdx >= 0 && endIdx > startIdx {
		// Replace existing section.
		return description[:startIdx] + wrapped + description[endIdx+len(gardenioMarkerEnd):]
	}

	// Append new section.
	if description != "" && !strings.HasSuffix(description, "\n") {
		description += "\n"
	}
	if description != "" {
		description += "\n"
	}
	return description + wrapped
}

// extractGardenioSection returns the content between the gardenio markers, or
// empty string if markers are not found.
func extractGardenioSection(description string) string {
	startIdx := strings.Index(description, gardenioMarkerStart)
	endIdx := strings.Index(description, gardenioMarkerEnd)
	if startIdx < 0 || endIdx < 0 || endIdx <= startIdx {
		return ""
	}
	content := description[startIdx+len(gardenioMarkerStart) : endIdx]
	return strings.TrimPrefix(content, "\n")
}

// syncMRDescription fetches the current MR description, splices the gardenio
// section, and updates it via the GitLab API. This is a no-op if the section
// content hasn't changed.
func syncMRDescription(ctx context.Context, gitlab *GitLabClient, projectPath string, mrIID int, section MRDescriptionSection, logger *slog.Logger) error {
	mr, err := gitlab.GetMergeRequestByPath(ctx, projectPath, mrIID)
	if err != nil {
		return fmt.Errorf("fetch MR for description sync: %w", err)
	}

	newContent := buildGardenioSection(section)
	existingContent := extractGardenioSection(mr.Description)

	if existingContent == newContent {
		logger.Debug("MR description section unchanged, skipping update",
			"project", projectPath, "mr_iid", mrIID)
		return nil
	}

	updated := spliceGardenioSection(mr.Description, newContent)
	if err := gitlab.UpdateMergeRequestDescription(ctx, projectPath, mrIID, updated); err != nil {
		return fmt.Errorf("update MR description: %w", err)
	}

	logger.Info("synced gardenio context to MR description",
		"project", projectPath, "mr_iid", mrIID, "bead", section.BeadID)
	return nil
}
