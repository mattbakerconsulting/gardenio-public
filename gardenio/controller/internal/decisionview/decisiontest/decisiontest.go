// Package decisiontest is the one acceptance decision every surface that
// answers a decision is golden-tested and UX-graded with (epic gd-hmwSI9lwKe
// step 6, gd-DiU8SB6TAM): the Gate's plot-application approval, as the Gate
// files it (cmd/beads-viewer lands_apply.go fileDecision and decisionContext),
// widened to five options so a surface's option row wraps, and asked with
// `gb decision create --evidence` naming a task whose comments carry two
// `gb image upload` links.
//
// It is the hard case for the epic's rule (a resolve control never renders
// without its context, above the options, with the evidence under it): a
// context long enough to fold behind Show more, Markdown with inline code, a
// bare URL and a fenced block, option descriptions, and pictures that load
// only through the viewer's /image/ proxy. The viewer goldens
// (cmd/beads-viewer), the /garden goldens (gardenhub), the Commons card
// (commons) and the Slack card (bridge) all read it from here, so they grade
// the same decision.
//
// Only tests import it.
package decisiontest

import (
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Prompt is the Gate's prompt for the application.
const Prompt = "Plot application: analytical-engine for ada-l/analytical-engine from ada-l"

// Title is the decision bead's title: the Gate titles the bead with its prompt.
const Title = Prompt

// Context is what the Gate's decisionContext writes for Application, byte for
// byte (TestGateFixtureIsTheGatesOwn in cmd/beads-viewer pins it): every
// value the applicant chose is fenced as code, only the Gate's own words are
// Markdown. It is over 1,000 characters, past every surface's fold.
const Context = "This application asks for a land called `analytical-engine`.\n\n" +
	"- Applicant: `ada@example.com` (GitHub `ada-l`)\n" +
	"- GitHub account: 2114 days old, 37 public repositories\n" +
	"- Repository: https://github.com/ada-l/analytical-engine (public)\n" +
	"- Credits asked for: 40\n" +
	"\nWhat they want to do:\n\n" +
	"```\n" +
	"Port the note G Bernoulli program to a modern runtime and keep a crew of\n" +
	"agents triaging the issue tracker while I write the paper. I expect two or\n" +
	"three agents working most weekdays, mostly on test coverage and docs, with\n" +
	"the occasional refactor of the card reader module, which is old and has no\n" +
	"tests at all. The mill and the store are separate packages; the store's\n" +
	"tests take about four minutes, so a crew would run them in CI, not locally.\n" +
	"```\n" +
	"\nApprove grants 25 credits on Sonnet 5.5 unless your reply says credits=<n> or model=opus. " +
	"Deny's reply is the reason the applicant reads. Ask's reply is emailed to them as a question. " +
	"Dismissing this files it again: deny instead."

// Application is the plot application Context was written for, as the
// fields of cmd/beads-viewer's landsApplication (the Gate's own type lives in
// package main, so the viewer test builds it from these).
var Application = map[string]string{
	"id":                "gd-GateApp07",
	"applicant_email":   "ada@example.com",
	"github_login":      "ada-l",
	"land_name":         "analytical-engine",
	"repo":              "ada-l/analytical-engine",
	"account_age_days":  "2114",
	"public_repos":      "37",
	"credits_requested": "40",
	"purpose": "Port the note G Bernoulli program to a modern runtime and keep a crew of agents triaging the issue " +
		"tracker while I write the paper. I expect two or three agents working most weekdays, mostly on test coverage and " +
		"docs, with the occasional refactor of the card reader module, which is old and has no tests at all. The mill and " +
		"the store are separate packages; the store's tests take about four minutes, so a crew would run them in CI, not locally.",
}

// DefaultCredits is the Gate's default grant the context names.
const DefaultCredits = 25

// Options is the decision's options as the daemon stores them: the Gate's
// three (Approve, Deny, Ask) plus two variants of Approve, five in all, each
// with a description and Approve recommended. Every surface adds its own
// free-text Other to them.
const Options = `[` +
	`{"id":"approve","short":"Approve","recommended":true,"artifact_type":"report",` +
	`"label":"Approve: provision the land with 25 credits on Sonnet 5.5. To change either, reply credits=<n> and/or model=opus.",` +
	`"description":"The land is created and the applicant is emailed a sign-in link."},` +
	`{"id":"approve-40","short":"Approve 40","artifact_type":"report",` +
	`"label":"Approve with the 40 credits they asked for, on Sonnet 5.5.",` +
	`"description":"As Approve, with the credits the application names."},` +
	`{"id":"approve-opus","short":"Approve on Opus","artifact_type":"report",` +
	`"label":"Approve with 25 credits on Opus 5.5.",` +
	`"description":"As Approve; the land's agents run on Opus, which spends credits about five times as fast."},` +
	`{"id":"ask","short":"Ask","artifact_type":"report",` +
	`"label":"Ask: your reply is emailed to the applicant as a question, and the application stays open.",` +
	`"description":"The decision is filed again when they reply."},` +
	`{"id":"deny","short":"Deny","artifact_type":"report",` +
	`"label":"Deny: your reply is the reason the applicant reads.",` +
	`"description":"Closes the application; no land is created."}` +
	`]`

// OptionShorts is each option's button text, in order.
var OptionShorts = []string{"Approve", "Approve 40", "Approve on Opus", "Ask", "Deny"}

// OptionDescriptions is each option's description, in order.
var OptionDescriptions = []string{
	"The land is created and the applicant is emailed a sign-in link.",
	"As Approve, with the credits the application names.",
	"As Approve; the land's agents run on Opus, which spends credits about five times as fast.",
	"The decision is filed again when they reply.",
	"Closes the application; no land is created.",
}

// EvidenceTitle is the evidence task's title, what a strip's "from" link reads.
const EvidenceTitle = "Grade the Gate's plot-application decision"

// ImageKeys are the two pictures' keys under the viewer's /image/ proxy, as
// `gb image upload` names them; ImageURLs the links it prints, in the order
// the strip shows them (the newer comment's first).
var (
	ImageKeys = []string{
		"coop/sessions/images/2026-10-31/7c41e0d2-after-mailbox-phone-light.png",
		"coop/sessions/images/2026-10-31/3b9f22a8-after-bead-desktop-dark.png",
	}
	ImageURLs = []string{
		"https://beads.example/image/" + ImageKeys[0],
		"https://beads.example/image/" + ImageKeys[1],
	}
	// ImageAlts is each picture's alt text: the Markdown alt of the first,
	// the file name of the bare second link.
	ImageAlts = []string{"after: the Mailbox row, phone, light", "3b9f22a8-after-bead-desktop-dark.png"}
)

// Decision is the acceptance decision with id, open, asked by lands-gate with
// `--evidence evidenceID`. Its requesting task is the application, which the
// fixture leaves out of every daemon, so a surface reads its evidence from
// evidenceID alone and must render whole without the other.
func Decision(id, evidenceID string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Title: Title, Kind: "data", Type: "decision", Status: "open", Priority: 1,
		Labels: []string{"project:gardenio"}, CreatedBy: "lands-gate",
		Fields: Fields(evidenceID),
	}
}

// Fields is the decision's field set, for a surface whose fixture builds its
// own bead around it.
func Fields(evidenceID string) map[string]string {
	return map[string]string{
		"prompt":          Prompt,
		"options":         Options,
		"context":         Context,
		"requested_by":    "lands-gate",
		"requesting_task": Application["id"],
		"evidence":        evidenceID,
	}
}

// Evidence is the task the decision names with --evidence: the acceptance
// step whose comments carry the screenshots.
func Evidence(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Title: EvidenceTitle, Type: "task", Status: "in_progress", Priority: 1,
		Labels: []string{"project:gardenio"},
	}
}

// Comments is the evidence task's comments as of now: an older note with no
// picture, then the grade's two `gb image upload` links, one as a Markdown
// image and one bare, the way the acceptance skills paste urls.txt.
func Comments(now time.Time) []beadsapi.Comment {
	return []beadsapi.Comment{
		{ID: 1, Author: "gardenio-6-acceptance", CreatedAt: now.Add(-50 * time.Minute),
			Text: "Shooting the Mailbox and the bead panel with the Gate fixture; pictures follow."},
		{ID: 2, Author: "gardenio-6-acceptance", CreatedAt: now.Add(-20 * time.Minute),
			Text: "commons-ux-acceptance mailbox: after 100% (accept)\n\n" +
				"![" + ImageAlts[0] + "](" + ImageURLs[0] + ")\n" +
				"after-bead-desktop-dark.png: " + ImageURLs[1]},
	}
}
