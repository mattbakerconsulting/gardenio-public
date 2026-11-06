package main

import (
	"net/http"

	"gardenio/controller/internal/oidcauth"
)

// The old viewer (Lands workbench-glass spec §2.3, §2.6, W-D7, step 5.13): the
// garden's Menu links GET /old-viewer, the way back to every page of the
// viewer that the garden does not open: the LATER pages, which have no
// Workbench panel yet; the retired pages, whose panel is now the default and
// whose page ?old=1 keeps (garden_retire.go oldViewerParam); and the DROP
// pages, which get no panel and stay served here for admins.
//
// It stays for as long as the viewer does (spec §7 Q5, answered by the owner
// 2026-10-25: indefinitely; beads-viewer is the administrative interface and
// is not deprecated, W-D7). Taking a page off the list is the owner's call.

// oldViewerPage is one page the old viewer lists: where it is and what it is
// for. Path is under the viewer's base path.
type oldViewerPage struct{ Name, Path, About string }

// oldViewerGroup is one of the list's sections.
type oldViewerGroup struct {
	Title, About string
	Pages        []oldViewerPage
}

// oldViewerGroups is the list, in spec order. TestOldViewerPagesSurviveTheSweep
// holds each page to a registered GET route that does not answer 302 into the
// garden with every retirement's redirect on.
var oldViewerGroups = []oldViewerGroup{
	{"Not in the garden yet", "These have no place in the garden yet (spec §2.3, LATER).", []oldViewerPage{
		{"Saved views", "/views", "Your saved filters over the beads."},
		{"Formulas", "/formulas", "The registry of reusable work recipes and their live instances. A plan's panel pours one."},
		{"Triggers", "/triggers", "What wakes the gardeners, and when."},
		{"Telepathy", "/telepathy", "The live cross-agent thinking feed."},
		{"Taints", "/taints", "Data-contamination markers."},
		{"People", "/people", "The people directory; each person's page, teams and forwarding."},
		{"Ratings", "/ratings", "How the agents' work has been rated."},
		{"Advice", "/advice", "Advice beads and what an agent is primed with."},
		{"Roles", "/roles", "Each role's agents and whether its config is valid."},
		{"Claude config", "/claude-config", "Each role's effective Claude Code config and where each value comes from."},
		{"Health", "/health", "The health tiles."},
		{"Live widgets", "/widget-types/", "The widget types the homepages draw."},
	}},
	{"Moved to the garden", "The garden opens these now; the old page is still here. Any bead, plan, project, bundle or gardener page keeps its old page with ?old=1 on its address.", []oldViewerPage{
		{"Plans", "/plans?old=1", "Every plan, its bulk edits and the master switch. A plan's critique, assistant and gardener are on its old cockpit: the plan's panel links it."},
		{"Decisions", "/decisions?old=1", "Every decision waiting, answered or dismissed."},
		{"Escalations", "/escalations?old=1", "Every escalation waiting on an approver."},
		{"Inbox", "/inbox?old=1", "Your operator inbox."},
		{"Search", "/search?old=1", "Every bead, by words."},
		{"Active", "/active?old=1", "What is in progress, across projects."},
		{"Feed", "/feed?old=1", "Everything that moved, newest first."},
		{"Gardeners", "/gardeners?old=1", "Every gardener loop."},
		{"Settings", "/settings?old=1", "Your settings and homepage."},
		{"Agents", "/agents?old=1", "The fleet's agents and their lifecycle events. The Glass's Wall shows them live."},
	}},
	{"Staying here", "These are not coming to the garden (spec §2.3, DROP); they stay for admins.", []oldViewerPage{
		{"Constellation", "/u/me?view=constellation", "Your inbox as a constellation."},
		{"Maps", "/maps", "The maps board."},
		{"Deck", "/deck", "The plans as a deck of cards."},
		{"Up", "/up", "The up board."},
		{"Extern", "/extern", "External objects, merge requests and tickets, as beads."},
		{"Peel catalog", "/peel-catalog", "Peel-eligible merge requests, ranked."},
		{"Cassettes", "/cassettes", "The LLM wire audit."},
		{"Tool latency", "/tool-latency", "The slowest agent tool calls."},
		{"Trust", "/trust", "Trusted throughput and its balancing metrics."},
		{"Incident intel", "/dev/incident-intel/gallery", "The incident-intel gallery."},
		{"Fleet", "/fleet", "The fleet's bridges and their sentinels."},
	}},
}

// handleOldViewer renders GET /old-viewer. It reads nothing itself; each page
// it links keeps its own route class.
func (s *Server) handleOldViewer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)
	s.render(w, r, "old_viewer.html", map[string]any{
		"Groups":      oldViewerGroups,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
	})
}
