package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gbPrimeBinary is the path to the gb binary used to render `gb prime` output
// for an arbitrary agent. The viewer image bakes /gb in via Dockerfile; local
// dev can override via GB_PRIME_BINARY.
func gbPrimeBinary() string {
	if v := os.Getenv("GB_PRIME_BINARY"); v != "" {
		return v
	}
	return "/gb"
}

// gbPrimeTimeout caps how long we wait for the gb subprocess. Prime renders
// are normally well under a second; 10s is a generous ceiling for slow daemons.
const gbPrimeTimeout = 10 * time.Second

// runGBPrime executes `gb prime --for <agentID>` with the agent's role and
// project propagated as env, returning the captured markdown stdout.
//
// daemonAddr/daemonToken come from the viewer's own beads daemon connection so
// the subprocess hits the same backend without re-reading config.
func runGBPrime(ctx context.Context, binary, agentID, role, project, daemonAddr, daemonToken string) ([]byte, error) {
	if agentID == "" {
		return nil, errors.New("agent id is required")
	}
	ctx, cancel := context.WithTimeout(ctx, gbPrimeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "prime", "--for", agentID)
	// Start from a minimal env — gb only reads a few well-known vars and we
	// don't want to leak the viewer's full process env into a subprocess.
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"BEADS_HTTP_ADDR=" + daemonAddr,
	}
	if daemonToken != "" {
		env = append(env, "BEADS_DAEMON_TOKEN="+daemonToken)
	}
	if role != "" {
		env = append(env, "BOAT_ROLE="+role)
	}
	if project != "" {
		env = append(env, "KD_PROJECT="+project)
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// stderr from gb is the size summary line — useful when debugging.
		return nil, fmt.Errorf("gb prime: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// agentPrimeView is the template view-model for /agents/{id}/prime.
type agentPrimeView struct {
	AgentID    string
	AgentName  string
	Role       string
	Project    string
	BodyHTML   template.HTML
	Error      string
	GeneratedAt string
}

// handleAgentPrime renders the live `gb prime` body for a target agent. The
// view is convenience-only — advice may change between renders, and we don't
// snapshot for an audit trail (operator decision in Slack thread).
func (s *Server) handleAgentPrime(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}

	view := agentPrimeView{
		AgentID:     id,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("loading agent bead for prime view", "id", id, "error", err)
		view.Error = "Failed to load agent bead."
		s.render(w, r, "prime_show.html", map[string]any{"View": view})
		return
	}
	if bead.Type != "agent" {
		// Non-agent beads can still be shown — prime does nothing useful for
		// them but the page renders the resulting (empty/identity-only) body
		// rather than 404'ing, which is more debuggable.
		s.logger.Info("rendering prime for non-agent bead", "id", id, "type", bead.Type)
	}

	view.AgentName = bead.Fields["agent"]
	if view.AgentName == "" {
		view.AgentName = bead.Title
	}
	view.Role = bead.Fields["role"]
	view.Project = bead.Fields["project"]
	if view.Project == "" {
		view.Project = projectFromLabels(bead.Labels)
	}

	daemonToken, err := s.daemonBearer(r.Context())
	if err != nil {
		s.logger.Error("resolving daemon credential for gb prime", "id", id, "error", err)
		view.Error = fmt.Sprintf("Failed to render prime: %v", err)
		s.render(w, r, "prime_show.html", map[string]any{"View": view})
		return
	}
	out, err := runGBPrime(r.Context(), gbPrimeBinary(), id, view.Role, view.Project, s.beadsHTTPAddr, daemonToken)
	if err != nil {
		s.logger.Error("running gb prime", "id", id, "error", err)
		view.Error = fmt.Sprintf("Failed to render prime: %v", err)
		s.render(w, r, "prime_show.html", map[string]any{"View": view})
		return
	}

	var buf bytes.Buffer
	if err := s.md.Convert(out, &buf); err != nil {
		s.logger.Error("rendering prime markdown", "id", id, "error", err)
		view.Error = "Failed to render prime markdown."
		s.render(w, r, "prime_show.html", map[string]any{"View": view})
		return
	}
	view.BodyHTML = template.HTML(buf.String())
	s.render(w, r, "prime_show.html", map[string]any{"View": view})
}
