// Package decision provides a Bubbletea TUI for monitoring and responding to
// decision points. It polls for pending decisions via the beadsapi HTTP client,
// displays them with urgency sorting, and lets users select options, add rationale,
// and resolve or dismiss decisions interactively.
package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const pollInterval = 5 * time.Second

// InputMode represents the current input mode.
type InputMode int

const (
	ModeNormal    InputMode = iota
	ModeRationale           // Entering rationale for selected option
)

// DecisionOption represents a single option within a decision.
type DecisionOption struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Short        string `json:"short"`
	Description  string `json:"description,omitempty"`
	ArtifactType string `json:"artifact_type,omitempty"`
}

// DecisionItem is a display-friendly wrapper around a decision point.
type DecisionItem struct {
	ID          string
	Prompt      string
	Options     []DecisionOption
	Urgency     string
	RequestedBy string
	RequestedAt time.Time
	Context     string
	Priority    int
}

// Model is the Bubbletea model for the decision watch TUI.
type Model struct {
	// Dimensions
	width, height int

	// Data
	decisions      []DecisionItem
	selected       int
	selectedOption int // 0 = none, 1-4 = option number

	// Input
	inputMode InputMode
	textInput textarea.Model
	rationale string

	// UI state
	keys           KeyMap
	help           help.Model
	showHelp       bool
	detailViewport viewport.Model
	filter         string // "high", "all", etc.
	err            error
	status         string

	// API client
	client *beadsapi.Client
	actor  string
}

// New creates a new decision TUI model.
func New(client *beadsapi.Client, actor string) *Model {
	ta := textarea.New()
	ta.Placeholder = "Enter rationale..."
	ta.SetHeight(3)
	ta.SetWidth(60)

	h := help.New()
	h.ShowAll = false

	return &Model{
		keys:           DefaultKeyMap(),
		help:           h,
		textInput:      ta,
		detailViewport: viewport.New(0, 0),
		filter:         "all",
		client:         client,
		actor:          actor,
	}
}

// SetFilter sets the urgency filter.
func (m *Model) SetFilter(filter string) {
	m.filter = filter
}

// Init initializes the model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchDecisions(),
		m.startPolling(),
		tea.SetWindowTitle("GB Decision Watch"),
	)
}

// --- Messages ---

type fetchDecisionsMsg struct {
	decisions []DecisionItem
	err       error
}

type tickMsg time.Time

type resolvedMsg struct {
	id  string
	err error
}

type dismissedMsg struct {
	id  string
	err error
}

// --- Commands ---

// fetchDecisions fetches pending decisions via the beadsapi client.
func (m *Model) fetchDecisions() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		details, err := client.ListDecisions(ctx, "open,in_progress", 50)
		if err != nil {
			return fetchDecisionsMsg{err: fmt.Errorf("fetch decisions: %w", err)}
		}

		var decisions []DecisionItem
		for _, dd := range details {
			// The "issue" field is the full bead; "decision" is a lightweight summary.
			b := dd.Issue
			if b == nil {
				b = dd.Decision
			}
			if b == nil {
				continue
			}
			item := DecisionItem{
				ID:          b.ID,
				Prompt:      b.Fields["prompt"],
				Urgency:     priorityToUrgency(b.Priority),
				RequestedBy: b.Fields["requested_by"],
				RequestedAt: b.UpdatedAt,
				Context:     b.Fields["context"],
				Priority:    b.Priority,
			}
			if item.Prompt == "" {
				item.Prompt = b.Title
			}

			// Parse options from fields JSON.
			if raw := b.Fields["options"]; raw != "" {
				var opts []DecisionOption
				if json.Unmarshal([]byte(raw), &opts) == nil {
					item.Options = opts
				}
			}

			decisions = append(decisions, item)
		}

		return fetchDecisionsMsg{decisions: decisions}
	}
}

// startPolling starts the poll ticker.
func (m *Model) startPolling() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// resolveDecision resolves a decision with the given option.
func (m *Model) resolveDecision(decisionID, optionID, rationale string) tea.Cmd {
	client := m.client
	actor := m.actor
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := client.ResolveDecision(ctx, decisionID, beadsapi.ResolveDecisionRequest{
			SelectedOption: optionID,
			ResponseText:   rationale,
			RespondedBy:    actor,
		})
		if err != nil {
			return resolvedMsg{id: decisionID, err: err}
		}
		return resolvedMsg{id: decisionID}
	}
}

// dismissDecision cancels/dismisses a decision.
func (m *Model) dismissDecision(decisionID, reason string) tea.Cmd {
	client := m.client
	actor := m.actor
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := client.CancelDecision(ctx, decisionID, reason, actor)
		if err != nil {
			return dismissedMsg{id: decisionID, err: err}
		}
		return dismissedMsg{id: decisionID}
	}
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.detailViewport.Width = msg.Width - 4
		m.detailViewport.Height = msg.Height/2 - 4
		m.textInput.SetWidth(msg.Width - 10)

	case tea.KeyMsg:
		// Handle input mode first
		if m.inputMode != ModeNormal {
			return m.handleInputMode(msg)
		}

		switch {
		case key.Matches(msg, m.keys.Quit), key.Matches(msg, m.keys.Cancel):
			return m, tea.Quit

		case key.Matches(msg, m.keys.Help):
			m.showHelp = !m.showHelp

		case key.Matches(msg, m.keys.Up):
			if m.selected > 0 {
				m.selected--
				m.selectedOption = 0
			}

		case key.Matches(msg, m.keys.Down):
			if m.selected < len(m.decisions)-1 {
				m.selected++
				m.selectedOption = 0
			}

		case key.Matches(msg, m.keys.Select1):
			m.selectedOption = 1

		case key.Matches(msg, m.keys.Select2):
			m.selectedOption = 2

		case key.Matches(msg, m.keys.Select3):
			m.selectedOption = 3

		case key.Matches(msg, m.keys.Select4):
			m.selectedOption = 4

		case key.Matches(msg, m.keys.Rationale):
			if m.selectedOption > 0 {
				m.inputMode = ModeRationale
				m.textInput.Focus()
				m.textInput.SetValue("")
				m.textInput.Placeholder = "Enter rationale (optional)..."
			}

		case key.Matches(msg, m.keys.Confirm):
			if m.selectedOption > 0 && len(m.decisions) > 0 && m.selected < len(m.decisions) {
				d := m.decisions[m.selected]
				if m.selectedOption <= len(d.Options) {
					optID := d.Options[m.selectedOption-1].ID
					cmds = append(cmds, m.resolveDecision(d.ID, optID, m.rationale))
					m.status = fmt.Sprintf("Resolving %s...", d.ID)
				}
			}

		case key.Matches(msg, m.keys.Refresh):
			cmds = append(cmds, m.fetchDecisions())
			m.status = "Refreshing..."

		case key.Matches(msg, m.keys.Dismiss):
			if len(m.decisions) > 0 && m.selected < len(m.decisions) {
				d := m.decisions[m.selected]
				cmds = append(cmds, m.dismissDecision(d.ID, "Dismissed via TUI"))
				m.status = fmt.Sprintf("Dismissing %s...", d.ID)
			}

		case key.Matches(msg, m.keys.FilterHigh):
			m.filter = "high"

		case key.Matches(msg, m.keys.FilterAll):
			m.filter = "all"
		}

	case fetchDecisionsMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.decisions = m.filterDecisions(msg.decisions)
			if m.selected >= len(m.decisions) {
				m.selected = max(0, len(m.decisions)-1)
			}
			m.status = fmt.Sprintf("Updated: %d pending", len(m.decisions))
		}

	case tickMsg:
		cmds = append(cmds, m.fetchDecisions())
		cmds = append(cmds, m.startPolling())

	case resolvedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.status = fmt.Sprintf("Error: %v", msg.err)
		} else {
			m.status = fmt.Sprintf("Resolved: %s", msg.id)
			m.selectedOption = 0
			m.rationale = ""
			cmds = append(cmds, m.fetchDecisions())
		}

	case dismissedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.status = fmt.Sprintf("Dismiss error: %v", msg.err)
		} else {
			m.status = fmt.Sprintf("Dismissed: %s", msg.id)
			m.selectedOption = 0
			cmds = append(cmds, m.fetchDecisions())
		}
	}

	// Update viewport
	var cmd tea.Cmd
	m.detailViewport, cmd = m.detailViewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// handleInputMode handles key presses in input mode.
func (m *Model) handleInputMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.inputMode = ModeNormal
		m.textInput.Blur()
		return m, nil

	case tea.KeyEnter:
		if m.inputMode == ModeRationale {
			m.rationale = m.textInput.Value()
			m.inputMode = ModeNormal
			m.textInput.Blur()

			// Auto-confirm if we have an option selected.
			if m.selectedOption > 0 && len(m.decisions) > 0 && m.selected < len(m.decisions) {
				d := m.decisions[m.selected]
				if m.selectedOption <= len(d.Options) {
					optID := d.Options[m.selectedOption-1].ID
					return m, m.resolveDecision(d.ID, optID, m.rationale)
				}
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

// filterDecisions filters and sorts decisions based on current filter.
func (m *Model) filterDecisions(decisions []DecisionItem) []DecisionItem {
	var result []DecisionItem

	if m.filter == "all" {
		result = decisions
	} else {
		for _, d := range decisions {
			if d.Urgency == m.filter {
				result = append(result, d)
			}
		}
	}

	// Sort by priority (lower = more urgent) then by time (newest first).
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].RequestedAt.After(result[j].RequestedAt)
	})

	return result
}

// View renders the TUI.
func (m *Model) View() string {
	return m.renderView()
}

// priorityToUrgency converts a numeric priority to an urgency string.
func priorityToUrgency(priority int) string {
	switch {
	case priority <= 1:
		return "high"
	case priority == 2:
		return "medium"
	default:
		return "low"
	}
}
