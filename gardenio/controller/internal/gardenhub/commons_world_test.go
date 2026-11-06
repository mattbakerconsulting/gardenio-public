package gardenhub

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/decisionview/decisiontest"
)

// The Commons pane's fixture land (Lands Commons I step 9, gd-b4IDsCKWbB): the gardenio land's four default channels and
// one user channel, #general holding a Slack wish with replies, people and agents talking, an agent card and a link, so the
// goldens and the pane walk (commons_browser_test.go) shoot the pane as the operator's land will look. Every time is fixed
// against now.

// commonsViewer is who the fixture reads and writes as: a member of the land (not the operator, whom membership never
// holds back), so a user channel it is not in offers Join.
const commonsViewer = "oidc:matt@example.com"

// wishSlack is a Slack wish with its whole family (commonstest.WishFamily, plan gd-gO1fTT7oGC step 1): replies, squawks, a
// card, a session, an agent, the task it gave rise to and its MR, and comments. A day old, so it sits above every other
// #general entry. The Commons reads it from its daemon, the bead view from the card world's beads (cardWorldRig).
const wishSlack = "gd-wishSlack"

func wishFamily(now time.Time) commonstest.Family {
	return commonstest.WishFamily(wishSlack, "gardenio", "gd-chGeneral", now.Add(-26*time.Hour))
}

func commonsWorld(now time.Time) *commonstest.Daemon {
	d := &commonstest.Daemon{Scopes: map[string][]string{}, Now: func() time.Time { return now }}
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	land := []string{"project:gardenio"}
	add := func(b *beadsapi.BeadDetail) {
		if b.Labels == nil {
			b.Labels = land
		}
		d.Put(b)
		d.Log("beads.bead.created", b, b.CreatedAt)
	}
	// The beads the land's messages and cards link, so each reads as its title (gd-YvdhZD2UtL); the lab land keeps
	// gd-AbCdEfGhIj missing from the daemon (F6), which reads "Bead not found". Put, not logged: they are no
	// conversation.
	for _, b := range [][2]string{
		{"gd-b4IDsCKWbB", "Commons pane in /garden"},
		{"gd-amX5uMx4KM", "Inbound card actions"},
		{"gd-jAcBpiVyPN", "Decision cards in the Commons"},
	} {
		d.Put(&beadsapi.BeadDetail{ID: b[0], Type: "task", Title: b[1], Status: "in_progress", Labels: land, CreatedAt: at(900)})
	}
	ch := func(id, name, kind, title, topic string, fields ...string) {
		f := map[string]string{"name": name, "channel_kind": kind}
		for i := 0; i+1 < len(fields); i += 2 {
			f[fields[i]] = fields[i+1]
		}
		add(&beadsapi.BeadDetail{ID: id, Type: "channel", Title: title, Description: topic, Status: "open", CreatedAt: at(600), Fields: f})
	}
	msg := func(id, channel, thread, author, text string, ago int, extra ...string) {
		f := map[string]string{"channel_id": channel, "author": author, "text": text}
		if thread != "" {
			f["thread_id"] = thread
		}
		for i := 0; i+1 < len(extra); i += 2 {
			f[extra[i]] = extra[i+1]
		}
		add(&beadsapi.BeadDetail{ID: id, Type: "message", Title: text, Status: "closed", CreatedAt: at(ago), Fields: f})
	}

	ch("gd-chGeneral", "general", "general", "General", "The whole land: wishes, news, anything", "mirror", "both")
	ch("gd-chCrew", "crew", "crew", "Crew", "Agent cards land here")
	ch("gd-chDecide", "decisions", "decisions", "Decisions", "")
	ch("gd-chEscal", "escalations", "escalations", "Escalations", "")
	ch("gd-chDesign", "design", "user", "Design", "Pixel art, palettes and the look of the farm", "members", `["oidc:flo@example.com"]`)

	msg("gd-msgHello", "gd-chGeneral", "", "oidc:flo@example.com", "Morning! The phone Projects sheet is live; tell me if a row reads wrong.", 190)
	add(&beadsapi.BeadDetail{ID: "gd-wishPorch", Type: "task", Title: "Porch chat should open the Commons thread", Status: "in_progress",
		Labels: []string{"thread", "project:gardenio"}, CreatedAt: at(150), CreatedBy: "oidc:matt@example.com",
		Fields: map[string]string{"channel_id": "gd-chGeneral", "slack_root_text": "Porch chat should open the Commons thread instead of the old /agents/chat page. See gd-b4IDsCKWbB.",
			"slack_thread_channel": "C0GARDEN01", "slack_thread_ts": "1792400000.000100"}})
	// the farm's plan A is bound to that wish, as a plan is to the Slack thread it was asked in: its Postcard goes there
	// (gd-WxRuyfdUtR). Put, not logged, so no stream replays it.
	d.Put(&beadsapi.BeadDetail{ID: "gd-planA", Type: "epic", Title: "Edge IdP rollout", Status: "in_progress", Labels: land, CreatedAt: at(600),
		Fields: map[string]string{"thread_id": "gd-wishPorch"}, Heraldry: "🌉🛡️⚓"})
	msg("gd-repPorch1", "gd-chGeneral", "gd-wishPorch", "agent:gardenio-29-the-commons-nec", "On it. The pane reads the same thread fragment, so the porch can show it in place.", 140)
	// the plan's card in its wish thread, its arms in the header (heraldry steps 3.3 and 3.4, the commonsfacearms golden). A
	// reply, so no unread badge moves (commons unreadBy counts top-level entries only).
	msg("gd-repPorchPlan", "gd-chGeneral", "gd-wishPorch", "agent:gardenio-plan-gardener", "Plan: Edge IdP rollout", 130, "kind", "card", "subject_id", "gd-planA")
	msg("gd-repPorch2", "gd-chGeneral", "gd-wishPorch", "oidc:matt@example.com", "Good. Keep the composer pinned above the keyboard on a phone.", 120)
	msg("gd-repPorch3", "gd-chGeneral", "gd-wishPorch", "agent:gardenio-29-the-commons-nec", "Done: it follows the visual viewport.\n\n- PR https://github.com/mattbakerconsulting/gardenio/pull/612 has the goldens\n- the composer stays above the keyboard on **iOS and Android**", 35)
	msg("gd-msgCard", "gd-chGeneral", "", "agent:gardenio-43-presence-who", "gardenio-43-presence-who is working on 4.3 Presence", 60, "kind", "card", "subject_id", "gd-agentPres1")
	msg("gd-msgDeploy", "gd-chGeneral", "", "oidc:flo@example.com", "Deploying 1c to the cluster after lunch.", 20)
	// attachments (gd-fAgk8mVsta): an image on the file route shows inline, the pdf beside it as a link
	msg("gd-msgSnap", "gd-chGeneral", "", "oidc:matt@example.com", "The farm at dusk, for the night palette pass.", 10,
		"files", `[{"name":"farm-at-dusk.png","mimetype":"image/png","size":`+strconv.Itoa(len(snapPNG))+`,"width":480,"height":270,"url":"https://garden.example`+snapPath+`"},`+
			`{"name":"night-palette.pdf","mimetype":"application/pdf","size":18432,"url":"https://garden.example/garden/c/f/gardenio/gd-chGeneral/fedcba9876543210fedcba9876543210/night-palette.pdf"}]`)

	// The agents the cards render (lands spec 2.5): gd-msgCard's at work, and one wrapped up with a card in #crew. Agents
	// and tasks are put without a created event: they are not conversation.
	d.Put(&beadsapi.BeadDetail{ID: "gd-taskPres", Type: "task", Title: "4.3 Presence: who is in the land and channel", Status: "in_progress",
		Labels: land, CreatedAt: at(70)})
	d.Put(&beadsapi.BeadDetail{ID: "gd-agentPres1", Type: "agent", Title: "gardenio-43-presence-who", Status: "open", Labels: land,
		CreatedAt: at(70), UpdatedAt: at(2), Fields: map[string]string{"agent": "gardenio-43-presence-who", "project": "gardenio",
			"role": "crew", "agent_state": "working", "task_id": "gd-taskPres", "live_cost_usd": "2.48", "live_turns": "57",
			"context_tokens": "142000", "operated_by": "oidc:matt@example.com", "spawned_by": "gardenio-plan-gardener"}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-agentUnread", Type: "agent", Title: "gardenio-45-read-markers", Status: "closed", Labels: land,
		CreatedAt: at(240), UpdatedAt: at(45), Fields: map[string]string{"agent": "gardenio-45-read-markers", "project": "gardenio",
			"role": "crew", "agent_state": "done", "total_cost_usd": "4.10", "total_request_count": "88", "context_tokens": "96000",
			"wrapup": `{"tldr":"Unread badges ship per channel and per land","accomplishments":"Each viewer's read cursor now lives on their user bead, so a badge counts what they have not read.\n\n| Surface | Before | After |\n|---|---|---|\n| channel list | no count | unread badge |\n| land rail | no count | land total |\n\n- read_cursors debounced to one write per 10 s\n- badges patched from the stream, no refetch","blockers":"Needs a release before the badge reaches the cluster","handoff_notes":"Push notifications build on the same cursor: start them now, or wait for the release?","pull_requests":["https://github.com/example/gardenio/pull/1654"]}`}})
	msg("gd-msgCardDone", "gd-chCrew", "", "agent:gardenio-45-read-markers", "Agent: gardenio-45-read-markers", 45, "kind", "card", "subject_id", "gd-agentUnread")

	// An agent that died mid-work (Commons II step 10, gd-zOFaT9VFUc): its card in #crew with the died face, the crash
	// notice and the death report in the card's thread, and a death-storm roll-up naming the deaths held back after it.
	d.Put(&beadsapi.BeadDetail{ID: "gd-agentDied", Type: "agent", Title: "gardenio-310-death-and-vqf", Status: "closed", Labels: land,
		CreatedAt: at(60), UpdatedAt: at(28), Fields: map[string]string{"agent": "gardenio-310-death-and-vqf", "project": "gardenio",
			"role": "crew", "agent_state": "done", "close_reason": "context_cancelled_involuntary", "total_cost_usd": "1.92",
			"controller_restart_reason": "account rotation deleted the pod",
			"total_request_count":       "41", "context_tokens": "88000", "operated_by": "oidc:matt@example.com"}})
	msg("gd-msgCardDied", "gd-chCrew", "", "agent:gardenio-310-death-and-vqf", "Agent: gardenio-310-death-and-vqf", 30, "kind", "card", "subject_id", "gd-agentDied")
	msg("gd-msgCrash", "gd-chCrew", "gd-msgCardDied", "agent:gardenio-bridge", "Agent crashed: gardenio-310-death-and-vqf", 29,
		"kind", "notice", "notice_subject", "gd-agentDied", "notice", "crash", "notice_agent", "gardenio-310-death-and-vqf",
		"notice_pod", "gardenio-310-death-and-vqf-7c9d4", "notice_pod_phase", "failed")
	msg("gd-msgDeath", "gd-chCrew", "gd-msgCardDied", "agent:gardenio-forensics", "Agent died: gardenio-310-death-and-vqf", 28,
		"kind", "notice", "notice_subject", "gd-agentDied", "notice", "death", "notice_agent", "gardenio-310-death-and-vqf",
		"notice_cause", "account rotation deleted the pod — the agent was mid-turn and did not `gb done`, so its claimed task was returned to the queue")
	msg("gd-msgStorm", "gd-chCrew", "", "agent:gardenio-forensics", "Death storm: 7 more agents died in gardenio (reports held back)", 22,
		"kind", "notice", "notice", "death_storm", "notice_count", "7", "notice_window", "5m0s",
		"notice_dead", `[{"id":"gd-agentS1","name":"gardenio-crew-storm-1"},{"id":"gd-agentS2","name":"gardenio-crew-storm-2"},{"id":"gd-agentS3","name":"gardenio-41-channel-seed"},{"id":"gd-agentS4","name":"gardenio-44-typing-indicators"},{"id":"gd-agentS5","name":"gardenio-1c-draw-gardeners"}]`)

	// #decisions (Commons II step 3, gd-jAcBpiVyPN): an open decision with its options, a tool-permission request waiting
	// for Allow or Deny, and a decision already answered. The subjects are put without a created event, like the agents.
	// The open decision asks the viewer (its operated_by: "Waiting on you"), the permission its agent's operator, flo
	// ("Waiting on flo", rubric C9, gd-yslNMe9RFk). It is the acceptance decision (decisiontest, gd-DiU8SB6TAM), its
	// --evidence a task of this land whose comment's two screenshots are drawn under the context.
	gate := decisiontest.Decision("gd-decGate", "gd-GateEvid01")
	gate.Labels, gate.Assignee, gate.CreatedAt, gate.UpdatedAt = land, "gardenio-33-decisions-and", at(30), at(30)
	gate.Fields["operated_by"] = commonsViewer
	d.Put(gate)
	ev := decisiontest.Evidence("gd-GateEvid01")
	ev.Labels, ev.CreatedAt = land, at(120)
	d.Put(ev)
	d.Comments = map[string][]beadsapi.Comment{"gd-GateEvid01": decisiontest.Comments(now)}
	// the application the context names, so its link reads as the application's title
	d.Put(&beadsapi.BeadDetail{ID: decisiontest.Application["id"], Type: "task", Title: "Plot application: analytical-engine", Status: "open", Labels: land, CreatedAt: at(130)})
	// The agent asking them, at its prompt: the open cards' See the screen opens its Glass (Lands spec 3.3, gd-YqMRIwn3Xi).
	d.Put(&beadsapi.BeadDetail{ID: "gd-agentDecide", Type: "agent", Title: "gardenio-33-decisions-and", Status: "open", Labels: land,
		CreatedAt: at(40), UpdatedAt: at(12), Fields: map[string]string{"agent": "gardenio-33-decisions-and", "project": "gardenio",
			"role": "crew", "agent_state": "prompt", "operated_by": "oidc:flo@example.com"}})
	msg("gd-msgDecGate", "gd-chDecide", "", "agent:gardenio-33-decisions-and", "Decision needed: "+decisiontest.Title, 30, "kind", "card", "subject_id", "gd-decGate")
	d.Put(&beadsapi.BeadDetail{ID: "gd-permHelm", Type: "permission", Title: "Permission: Bash for gardenio-33-decisions-and", Status: "open",
		Labels: land, CreatedAt: at(12), Fields: map[string]string{"agent": "gardenio-33-decisions-and", "request_id": "kqzpe", "tool_name": "Bash",
			"description": "Regenerate the garden goldens", "input_preview": "make update-garden-goldens"}})
	msg("gd-msgPermHelm", "gd-chDecide", "", "agent:gardenio-33-decisions-and", "", 12, "kind", "card", "subject_id", "gd-permHelm")
	d.Put(&beadsapi.BeadDetail{ID: "gd-decTheme", Type: "decision", Title: "Keep Slack's card themes?", Status: "closed", Priority: 2,
		Labels: land, Assignee: "gardenio-32-agent-cards", CreatedAt: at(90), UpdatedAt: at(80), Fields: map[string]string{
			"prompt": "Keep the Garden and Business card themes in the Commons?", "options": `["Keep both","One theme, light and dark"]`,
			"chosen": "One theme, light and dark", "rationale": "Spec C8: themes stay a Slack concern", "responded_by": "oidc:matt@example.com"}})
	msg("gd-msgDecTheme", "gd-chDecide", "", "agent:gardenio-32-agent-cards", "Decision needed: Keep Slack's card themes?", 90, "kind", "card", "subject_id", "gd-decTheme")
	// The other closed faces (Commons II step 11, gd-vyGbXELqgH): a dismissed decision, one sent back to be escalated, and
	// a permission allowed and one denied, as cards in a thread the viewer started (placement rule 1). The viewer has the
	// last word there, so the thread changes no unread badge.
	msg("gd-msgNight", "gd-chDecide", "", commonsViewer, "Last night's asks, answered before the standup", 200)
	d.Put(&beadsapi.BeadDetail{ID: "gd-decFonts", Type: "decision", Title: "Rebuild the agent image for the font pins?", Status: "closed", Priority: 2,
		Labels: land, Assignee: "gardenio-1a-looking-glass", CreatedAt: at(199), UpdatedAt: at(190), Fields: map[string]string{
			"prompt": "Rebuild the agent image so the goldens' Noto pins are on every pod?", "options": `["Rebuild tonight","Pin in the harness"]`,
			"canceled": "true", "canceled_by": "oidc:matt@example.com", "cancel_reason": "Superseded: the harness pins the fonts itself"}})
	msg("gd-msgDecFonts", "gd-chDecide", "gd-msgNight", "agent:gardenio-1a-looking-glass", "Decision needed: Rebuild the agent image for the font pins?", 199, "kind", "card", "subject_id", "gd-decFonts")
	d.Put(&beadsapi.BeadDetail{ID: "gd-decSpawn", Type: "decision", Title: "Let guests spawn agents?", Status: "closed", Priority: 1,
		Labels: land, Assignee: "gardenio-42-land-roles", CreatedAt: at(198), UpdatedAt: at(189), Fields: map[string]string{
			"prompt": "May a guest in a land spawn an agent with an @gardenio mention?", "options": `["Yes","No"]`,
			"chosen": "returned_to_agent_for_escalation", "responded_by": "oidc:matt@example.com",
			"rationale": "The owner decides who may spend the purse: escalate it"}})
	msg("gd-msgDecSpawn", "gd-chDecide", "gd-msgNight", "agent:gardenio-42-land-roles", "Decision needed: Let guests spawn agents?", 198, "kind", "card", "subject_id", "gd-decSpawn")
	d.Put(&beadsapi.BeadDetail{ID: "gd-permEdit", Type: "permission", Title: "Permission: Edit for gardenio-29-the-commons-nec", Status: "closed",
		Labels: land, CreatedAt: at(197), Fields: map[string]string{"agent": "gardenio-29-the-commons-nec", "request_id": "w3hfa", "tool_name": "Edit",
			"description": "Pin the composer above the keyboard", "input_preview": "internal/gardenhub/web/garden.css",
			"behavior": "allow", "answered_by": "oidc:matt@example.com"}})
	msg("gd-msgPermEdit", "gd-chDecide", "gd-msgNight", "agent:gardenio-29-the-commons-nec", "", 197, "kind", "card", "subject_id", "gd-permEdit")
	d.Put(&beadsapi.BeadDetail{ID: "gd-permDrop", Type: "permission", Title: "Permission: Bash for gardenio-crew-rbac", Status: "closed",
		Labels: land, CreatedAt: at(196), Fields: map[string]string{"agent": "gardenio-crew-rbac", "request_id": "p8vnc", "tool_name": "Bash",
			"description": "Clean up the e2e namespace", "input_preview": "kubectl delete namespace gardenio-e2e",
			"behavior": "deny", "answered_by": "oidc:matt@example.com"}})
	msg("gd-msgPermDrop", "gd-chDecide", "gd-msgNight", "agent:gardenio-crew-rbac", "", 196, "kind", "card", "subject_id", "gd-permDrop")
	msg("gd-repNight", "gd-chDecide", "gd-msgNight", commonsViewer, "All four answered; the guest spawn question goes to the owner.", 195)

	// #escalations (Commons II step 4, gd-1oIKTXObpw): a merge waiting for an operator, an approved deploy running, a
	// secret read done (only its redacted summary shows) and a rejected grant. The viewer has read the channel up to its
	// newest owner card, so the cards change no unread badge.
	d.Put(&beadsapi.BeadDetail{ID: "gd-escRbac", Type: "escalation", Title: "Grant cluster-admin to the crew role", Status: "closed", Priority: 2,
		Labels: land, CreatedAt: at(110), Fields: map[string]string{"operation_type": "rbac_grant", "requesting_agent": "gardenio-crew-rbac",
			"rejected_by": "oidc:matt@example.com", "rejection_reason": "Too broad: ask for the one namespace the job writes to."}})
	msg("gd-msgEscRbac", "gd-chEscal", "", "agent:gardenio-crew-rbac", "Escalation request: Grant cluster-admin to the crew role (rbac_grant)", 110, "kind", "card", "subject_id", "gd-escRbac")
	d.Put(&beadsapi.BeadDetail{ID: "gd-escAudit", Type: "escalation", Title: "Read the k3s API audit log for the last day", Status: "closed", Priority: 1,
		Labels: land, CreatedAt: at(70), Fields: map[string]string{"operation_type": "audit_read", "sensitivity": "secret",
			"requesting_agent": "gardenio-ship-k3s-api", "approved_by_email": "matt@example.com", "result_status": "success",
			"executor": "gardenio-infra", "redacted_summary": "No writes from outside the fleet; 3 reads by the backup job.", "result": `{"rows":"withheld"}`}})
	msg("gd-msgEscAudit", "gd-chEscal", "", "agent:gardenio-ship-k3s-api", "Escalation request: Read the k3s API audit log for the last day (audit_read)", 70, "kind", "card", "subject_id", "gd-escAudit")
	d.Put(&beadsapi.BeadDetail{ID: "gd-escHelm", Type: "escalation", Title: "Enable the Slack test-user session on the e2e namespace", Status: "open", Priority: 1,
		Labels: land, CreatedAt: at(40), Fields: map[string]string{"operation_type": "helm_deploy_values", "execution_target": "server",
			"requesting_agent": "gardenio-slack-e2e", "approved_by": "gd-userMatt", "approved_by_email": "matt@example.com", "approval_method": "web_ui"}})
	msg("gd-msgEscHelm", "gd-chEscal", "", "agent:gardenio-slack-e2e", "Escalation request: Enable the Slack test-user session on the e2e namespace (helm_deploy_values)", 40, "kind", "card", "subject_id", "gd-escHelm")
	d.Put(&beadsapi.BeadDetail{ID: "gd-escMerge", Type: "escalation", Title: "Merge PR #647: decision cards in the Commons", Status: "open", Priority: 0,
		Labels: land, CreatedAt: at(8), Fields: map[string]string{"operation_type": "gitlab_mr_merge", "execution_target": "local",
			"sensitivity": "internal", "requesting_agent": "gardenio-33-decisions-and", "requesting_task": "gd-jAcBpiVyPN",
			"pingback_count": "1", "pingback_reason": "3.4 is waiting on it",
			"context":                 "CI is green and the Slack payload golden is unchanged. Merging unblocks 3.4 (escalation cards).",
			"credential_requirements": `[{"type":"github_token","identifier":"merge"}]`,
			"operation_spec":          `{"mr_url":"https://github.com/mattbakerconsulting/gardenio/pull/647","mr_iid":647}`}})
	msg("gd-msgEscMerge", "gd-chEscal", "", "agent:gardenio-33-decisions-and", "Escalation request: Merge PR #647: decision cards in the Commons (gitlab_mr_merge)", 8, "kind", "card", "subject_id", "gd-escMerge")
	// Audience (gd-bmOiteQi36): the merge above needs a credential and the audit read is secret, so both are the owner's
	// and show in full; the grant and the deploy are the operators' and this plain merge the desk's, so the three fold into
	// "In hand: 3 (desk 1, operator 2)". The desk merge is newer than the viewer's read position but, in hand, counts no
	// unread.
	d.Put(&beadsapi.BeadDetail{ID: "gd-escDesk", Type: "escalation", Title: "Merge PR #712: the bead view's link colours", Status: "open", Priority: 1,
		Labels: land, CreatedAt: at(5), Fields: map[string]string{"operation_type": "gitlab_mr_merge", "requesting_agent": "gardenio-bead-links",
			"operation_spec": `{"mr_url":"https://github.com/mattbakerconsulting/gardenio/pull/712"}`}})
	msg("gd-msgEscDesk", "gd-chEscal", "", "agent:gardenio-bead-links", "Escalation request: Merge PR #712: the bead view's link colours (gitlab_mr_merge)", 5, "kind", "card", "subject_id", "gd-escDesk")
	d.Cursors = map[string]map[string]string{commonsViewer: {"gd-chEscal": "gd-msgEscMerge"}}

	// The lab land's #stress (commons-ux-acceptance's commonsstress surface): the rubric's fixtures F1 (a 1,200-char body, a
	// 90-char agent name), F2 (a long URL, a SHA, a branch, a file path, a bare id), F3 (thirty-odd messages, a thread of 50
	// replies) and F4 (ZWJ emoji, a flag, VS16, CJK, Arabic, a combining accent).
	lab := []string{"project:lab"}
	add(&beadsapi.BeadDetail{ID: "lb-chStress", Type: "channel", Title: "Stress", Description: strings.Repeat("A topic that runs on and on ", 5), Status: "open",
		Labels: lab, CreatedAt: at(900), Fields: map[string]string{"name": "stress", "channel_kind": "general"}})
	labMsg := func(id, thread, author, text string, ago int) {
		f := map[string]string{"channel_id": "lb-chStress", "author": author, "text": text}
		if thread != "" {
			f["thread_id"] = thread
		}
		add(&beadsapi.BeadDetail{ID: id, Type: "message", Title: text, Status: "closed", Labels: lab, CreatedAt: at(ago), Fields: f})
	}
	longAgent := "agent:gardenio-crew-" + strings.Repeat("very-long-agent-name-", 4)[:70]
	url := "https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01234567"
	for i := 0; i < 30; i++ {
		labMsg("lb-filler"+strconv.Itoa(100+i), "", "oidc:flo@example.com", "Filler message number "+strconv.Itoa(i+1)+".", 800-i*10)
	}
	labMsg("lb-msgLong", "", longAgent, strings.Repeat("A long body sentence so the message runs well past the fold of a phone screen. ", 15), 400)
	labMsg("lb-msgTokens", "", "oidc:田中花子@example.com", "Merge request "+url+" is the change. Head SHA 0123456789abcdef0123456789abcdef01234567 on branch "+
		"agent/some-very-long-branch-name/gd-AbCdEfGhIj; the bare id gd-AbCdEfGhIj names its task, and the file is "+
		"gardenio/controller/internal/gardenhub/web/garden.css:364.", 300)
	labMsg("lb-msgGlyphs", "", "oidc:مريم@example.com", "🧑‍🌾 Farmer note, 🇳🇿 flag, ⚠️ warning with VS16, 田中花子 and مريم reviewed it; café with a combining accent.", 200)
	// A farm snapshot (gd-GbF2JS04h5) with no words: its picture's long name, and a caption whose place is a long plan id,
	// named by the long title it was framed with (gd-YvdhZD2UtL).
	labMsg("lb-msgSnap", "", "oidc:田中花子@example.com", "", 150)
	d.Beads["lb-msgSnap"].Title = "Snapshot of the farm at lab/gd-AbCdEfGhIjKlMnOpQrStUv"
	d.Beads["lb-msgSnap"].Fields = map[string]string{"channel_id": "lb-chStress", "author": "oidc:田中花子@example.com",
		"files": `[{"name":"farm-lab-lab-gd-AbCdEfGhIjKlMnOpQrStUv-20261019-2340.png","mimetype":"image/png","size":2934112,"width":480,"height":270,"url":"http://127.0.0.1` + labSnapPath + `"}]`,
		"snap":  `{"kind":"farm","url":"http://127.0.0.1/garden?at=lab/gd-AbCdEfGhIjKlMnOpQrStUv&z=3","at":"lab/gd-AbCdEfGhIjKlMnOpQrStUv","z":3,"who":"all","night":true,"framed":[{"kind":"project","id":"lab"},{"kind":"plan","id":"gd-AbCdEfGhIjKlMnOpQrStUv","title":"Lab: a plan whose title runs well past the width of a phone's caption line"},{"kind":"plan","id":"gd-1"},{"kind":"bead","id":"gd-2"},{"kind":"bead","id":"gd-3"}]}`}
	// A set of pictures (WB-18 and WB-19, check-in gd-1lvbsJyEXb): a reply in the snapshot's thread with three image
	// attachments, the farm at dusk, midnight and dawn, so the preview and the viewer can be shot cycling a Commons set. A
	// reply, so #stress keeps its unread count.
	labMsg("lb-msgPics", "lb-msgSnap", "oidc:flo@example.com", "Three passes of the night palette: dusk, midnight, dawn.", 140)
	d.Beads["lb-msgPics"].Fields["files"] = `[` + strings.Join(func() []string {
		var fs []string
		for _, p := range labPicPaths {
			fs = append(fs, `{"name":"`+p[strings.LastIndex(p, "/")+1:]+`","mimetype":"image/png","size":24576,"width":480,"height":270,"url":"http://127.0.0.1`+p+`"}`)
		}
		return fs
	}(), ",") + `]`
	for i := 0; i < 50; i++ {
		labMsg("lb-rep"+strconv.Itoa(100+i), "lb-msgTokens", "oidc:flo@example.com", "Reply "+strconv.Itoa(i+1)+" in a long thread.", 290-i*5)
	}
	// F1 on an agent card: a 200-char task, a 90-char agent name and a long wrap-up, oldest in the channel so the first
	// screen shows them.
	stressAgent := strings.TrimPrefix(longAgent, "agent:")
	d.Put(&beadsapi.BeadDetail{ID: "lb-taskLong", Type: "task", Status: "in_progress", Labels: lab, CreatedAt: at(500),
		Title: strings.Repeat("Make the card hold a task title that keeps going ", 5)[:200]})
	d.Put(&beadsapi.BeadDetail{ID: "lb-agentLong", Type: "agent", Title: stressAgent, Status: "open", Labels: lab, CreatedAt: at(500), UpdatedAt: at(1),
		Fields: map[string]string{"agent": stressAgent, "project": "lab", "role": "crew", "agent_state": "rate_limited", "task_id": "lb-taskLong",
			"live_cost_usd": "123.456", "live_turns": "1234", "context_tokens": "987654", "operated_by": "oidc:田中花子@example.com", "spawned_by": url}})
	labMsg("lb-cardLong", "", longAgent, "Agent: "+stressAgent, 860)
	d.Beads["lb-cardLong"].Fields["kind"], d.Beads["lb-cardLong"].Fields["subject_id"] = "card", "lb-agentLong"
	// The agent's squawks in its card's thread, as gb squawk writes them (gd-fIM0zNikOd): no author field, created by the
	// agent's bare 90-char name, F1 a long one, F2 a URL, SHA and bead id, F4 emoji and non-Latin text.
	for i, text := range []string{
		"Claimed the task; reading the squawk path first.",
		strings.Repeat("A progress squawk that keeps going so it wraps well past one line on a phone. ", 8),
		"PR is up: " + url + " at 0123456789abcdef0123456789abcdef01234567, closes gd-AbCdEfGhIj.",
		"🧑‍🌾 田中花子 and مريم asked for café-style notes ⚠️ done.",
	} {
		id := "lb-squawk" + strconv.Itoa(i+1)
		add(&beadsapi.BeadDetail{ID: id, Type: "message", Title: text, Status: "closed", CreatedBy: stressAgent, CreatedAt: at(20 - 4*i),
			Labels: []string{"squawk", "from:" + stressAgent, "project:lab"},
			Fields: map[string]string{"source_agent": stressAgent, "text": text, "thread_id": "lb-cardLong", "channel_id": "lb-chStress"}})
	}
	d.Put(&beadsapi.BeadDetail{ID: "lb-agentDone", Type: "agent", Title: stressAgent + "-2", Status: "closed", Labels: lab, CreatedAt: at(500), UpdatedAt: at(5),
		Fields: map[string]string{"agent": stressAgent + "-2", "project": "lab", "role": "crew", "agent_state": "failed", "close_reason": "pod evicted",
			"wrapup": `{"tldr":"` + strings.Repeat("A tldr that will not stop ", 12) + `","accomplishments":"` + strings.Repeat("Did a thing, then another. ", 30) + `","blockers":"` + url + `","handoff_notes":"🧑‍🌾 田中花子 مريم café"}`}})
	labMsg("lb-cardDone", "", longAgent, "Agent: "+stressAgent+"-2", 850)
	d.Beads["lb-cardDone"].Fields["kind"], d.Beads["lb-cardDone"].Fields["subject_id"] = "card", "lb-agentDone"

	// Two tenant lands for the Lands kill switch (Track B step 10.5), plain data so their goldens render the same on any
	// machine: meadow, shot as the operator sees it with Freeze open (commonsfreeze), and orchard, frozen, shot as one of
	// its members sees it, the composer given way to the frozen banner (commonsfrozen).
	add(&beadsapi.BeadDetail{ID: "md-chGeneral", Type: "channel", Title: "General", Description: "The meadow's whole land", Status: "open",
		Labels: []string{"project:meadow"}, CreatedAt: at(900), Fields: map[string]string{"name": "general", "channel_kind": "general"}})
	add(&beadsapi.BeadDetail{ID: "md-msg1", Type: "message", Title: "hello", Status: "closed", Labels: []string{"project:meadow"}, CreatedAt: at(70),
		Fields: map[string]string{"channel_id": "md-chGeneral", "author": "oidc:bo@meadow.example", "text": "Wildflower seed is in; the steward is planting the north strip."}})
	orchard := []string{"project:orchard"}
	add(&beadsapi.BeadDetail{ID: "or-chGeneral", Type: "channel", Title: "General", Description: "The orchard's whole land", Status: "open",
		Labels: orchard, CreatedAt: at(900), Fields: map[string]string{"name": "general", "channel_kind": "general"}})
	for i, m := range []struct{ id, author, text string }{
		{"or-msg1", "oidc:ann@orchard.example", "Can an agent prune the old branches in the repo?"},
		{"or-msg2", "agent:orchard-steward", "On it: three stale branches found, a PR to remove them is up."},
	} {
		add(&beadsapi.BeadDetail{ID: m.id, Type: "message", Title: m.text, Status: "closed", Labels: orchard, CreatedAt: at(90 - 10*i),
			Fields: map[string]string{"channel_id": "or-chGeneral", "author": m.author, "text": m.text}})
	}
	// F1 and F2 on the death faces (gd-zOFaT9VFUc): a 90-char agent that died mid-work, its crash notice and a long
	// death report in its card's thread, and a storm roll-up naming more agents than it shows.
	d.Put(&beadsapi.BeadDetail{ID: "lb-agentDied", Type: "agent", Title: stressAgent + "-3", Status: "closed", Labels: lab, CreatedAt: at(500), UpdatedAt: at(6),
		Fields: map[string]string{"agent": stressAgent + "-3", "project": "lab", "role": "crew", "agent_state": "done",
			"close_reason": "context_cancelled_involuntary", "controller_restart_reason": strings.Repeat("stranded-account recovery deleted the pod ", 3)}})
	labMsg("lb-cardDied", "", longAgent, "Agent: "+stressAgent+"-3", 840)
	d.Beads["lb-cardDied"].Fields["kind"], d.Beads["lb-cardDied"].Fields["subject_id"] = "card", "lb-agentDied"
	labMsg("lb-crashDied", "lb-cardDied", "agent:gardenio-bridge", "Agent crashed: "+stressAgent+"-3", 7)
	for k, v := range map[string]string{"kind": "notice", "notice_subject": "lb-agentDied", "notice": "crash", "notice_agent": stressAgent + "-3",
		"notice_pod": stressAgent + "-3-7c9d4f8b6-x2x9q", "notice_pod_phase": "failed"} {
		d.Beads["lb-crashDied"].Fields[k] = v
	}
	labMsg("lb-deathDied", "lb-cardDied", "agent:gardenio-forensics", "Agent died: "+stressAgent+"-3", 6)
	for k, v := range map[string]string{"kind": "notice", "notice_subject": "lb-agentDied", "notice": "death", "notice_agent": stressAgent + "-3",
		"notice_cause": "the pod was deleted mid-turn; see " + url + " and gd-AbCdEfGhIj. " + strings.Repeat("The claimed task went back to the queue. ", 12)} {
		d.Beads["lb-deathDied"].Fields[k] = v
	}
	var storm []string
	for i := 0; i < 15; i++ {
		storm = append(storm, `{"id":"lb-dead`+strconv.Itoa(10+i)+`","name":"`+stressAgent+`-s`+strconv.Itoa(i)+`"}`)
	}
	labMsg("lb-stormDied", "", "agent:gardenio-forensics", "Death storm: 40 more agents died in lab (reports held back)", 5)
	for k, v := range map[string]string{"kind": "notice", "notice": "death_storm", "notice_count": "40", "notice_window": "5m0s",
		"notice_dead": "[" + strings.Join(storm, ",") + "]"} {
		d.Beads["lb-stormDied"].Fields[k] = v
	}
	// F1/F2/F6 on decision and permission cards (gd-jAcBpiVyPN): a 1,200-char prompt, eight long options, a 1,100-char
	// Markdown context with a long URL (it folds behind Show more, gd-OSI1uK35Wa), an unbroken 500-char tool input, and a card whose subject is missing from the daemon.
	var opts []string
	for i := 1; i <= 8; i++ {
		opts = append(opts, `{"id":"o`+strconv.Itoa(i)+`","short":"`+strings.Repeat("Option title that wraps ", 4)+strconv.Itoa(i)+`","description":"`+strings.Repeat("A description long enough to be cut. ", 6)+`"}`)
	}
	d.Put(&beadsapi.BeadDetail{ID: "lb-decLong", Type: "decision", Title: "long decision", Status: "open", Priority: 0, Labels: lab,
		Assignee: stressAgent, CreatedAt: at(870), UpdatedAt: at(870), Fields: map[string]string{
			"prompt": strings.Repeat("A decision prompt that keeps asking the same question in more words. ", 17), "context": "## Why it is asked\n\nThe chart's values schema changed under " + url + " and the gate needs **one** answer before " +
				"gd-AbCdEfGhIj can land. " + strings.Repeat("The background goes on at length, the way an agent writes when it wants the decision made once. ", 9) +
				"\n\n- `helm lint` passes\n- the golden diff is empty\n\n> Merging re-cuts the 16:00Z window.",
			"options": "[" + strings.Join(opts, ",") + "]"}})
	labMsg("lb-cardDec", "", longAgent, "Decision needed", 870)
	d.Beads["lb-cardDec"].Fields["kind"], d.Beads["lb-cardDec"].Fields["subject_id"] = "card", "lb-decLong"
	d.Put(&beadsapi.BeadDetail{ID: "lb-permLong", Type: "permission", Title: "long permission", Status: "open", Labels: lab, CreatedAt: at(868),
		Fields: map[string]string{"agent": stressAgent, "request_id": "zzzzz", "tool_name": "Bash", "description": "Run " + url,
			"input_preview": strings.Repeat("x", 500)}})
	labMsg("lb-cardPerm", "", longAgent, "Permission needed", 868)
	d.Beads["lb-cardPerm"].Fields["kind"], d.Beads["lb-cardPerm"].Fields["subject_id"] = "card", "lb-permLong"
	labMsg("lb-cardGone", "", longAgent, "Decision needed: its bead is gone", 866)
	d.Beads["lb-cardGone"].Fields["kind"], d.Beads["lb-cardGone"].Fields["subject_id"] = "card", "lb-decMissing"
	// F1/F2 on an escalation card (gd-1oIKTXObpw): a 300-char title, an unbroken 60-char operation, eight credentials, a
	// 1,200-char context with a long URL, a long merge request URL and a long pingback reason.
	var creds []string
	for i := 1; i <= 8; i++ {
		creds = append(creds, `{"type":"credential_with_a_long_name_`+strconv.Itoa(i)+`"}`)
	}
	d.Put(&beadsapi.BeadDetail{ID: "lb-escLong", Type: "escalation", Title: strings.Repeat("An escalation title that asks for a lot ", 8)[:300],
		Status: "open", Priority: 0, Labels: lab, CreatedAt: at(865), Fields: map[string]string{
			"operation_type": strings.Repeat("op_", 20), "execution_target": "service_account", "sensitivity": "sensitive",
			"requesting_agent": stressAgent, "requesting_task": "lb-taskLong", "pingback_count": "12",
			"pingback_reason":         strings.Repeat("Still waiting on the operator, again. ", 6),
			"context":                 strings.Repeat("The context of an escalation that explains itself at length. ", 18) + url,
			"credential_requirements": "[" + strings.Join(creds, ",") + "]",
			"operation_spec":          `{"mr_url":"` + url + `"}`}})
	labMsg("lb-cardEsc", "", longAgent, "Escalation request", 865)
	d.Beads["lb-cardEsc"].Fields["kind"], d.Beads["lb-cardEsc"].Fields["subject_id"] = "card", "lb-escLong"
	// Arms on cards (heraldry 3.3): an epic's card, the faceless kind, with its picture in its header. It is the oldest
	// filler made a card, so #stress keeps its bead count (the unread window) and its unread count (the Commons bar's badge
	// in every golden). The F1 agent, decision and escalation above bear arms too, which v1 refuses them (heraldry spec
	// R6), so the 24 px slot is graded beside the longest headers for the day R6 widens.
	d.Put(&beadsapi.BeadDetail{ID: "lb-planArms", Type: "epic", Title: "Plan with arms", Status: "open", Labels: lab, CreatedAt: at(880),
		Heraldry: "purple 🦊 ⚓ 🔥 📦 🧭"})
	d.Beads["lb-filler100"].Title, d.Beads["lb-filler100"].Fields["text"] = "Plan card", "Plan card"
	d.Beads["lb-filler100"].Fields["kind"], d.Beads["lb-filler100"].Fields["subject_id"] = "card", "lb-planArms"
	d.Beads["lb-agentLong"].Heraldry = "blue 🦊 . 🔥 . 📦"
	// The F1 wrap-up's agent bears none, so its card wears its task's epic's (gd-1iJEc1nYqm): lb-planArms'.
	d.Put(&beadsapi.BeadDetail{ID: "lb-taskDone", Type: "task", Title: "Wrap-up cards wear their plan's arms", Status: "closed", Labels: lab, CreatedAt: at(500)})
	d.Beads["lb-agentDone"].Fields["task_id"] = "lb-taskDone"
	d.Link("lb-taskDone", "lb-planArms", "parent-child")
	d.Beads["lb-decLong"].Heraldry = "gold . . ⚓ . ."
	d.Beads["lb-escLong"].Heraldry = "red 🧭 📦 . . ."

	// The lands' people, for the @ list (Lands Commons III step 6). gardenio's, the viewer among them, are Latin only:
	// the goldens pin Latin fonts, and any other script falls back to whatever the machine has. lab's carry the stress
	// names (F1): two other scripts, and a local part and a name long enough to wrap.
	d.Members = map[string][]beadsapi.LandMember{
		"gardenio": {
			{Principal: commonsViewer, Name: "matt"}, {Principal: "oidc:flo@example.com", Name: "Flo Reyes"},
			{Principal: "oidc:sam@example.com", Name: "sam"},
		},
		// meadow's, for the People lightbox's Members (its owner, the viewer, and two people an admin may Become; plan
		// gd-0SUIdKXWV3 step 3)
		"meadow": {
			{Principal: commonsViewer, Name: "matt", Role: "owner"}, {Principal: "oidc:phono@example.com", Name: "phonobaloon", Role: "member"},
			{Principal: "oidc:rue@example.com", Name: "Rue Okafor", Role: "guest"},
		},
		"lab": {
			{Principal: commonsViewer, Name: "matt"}, {Principal: "oidc:田中花子@example.com", Name: "田中花子"},
			{Principal: "oidc:مريم@example.com", Name: "مريم"},
			{Principal: "oidc:" + strings.Repeat("a-very-long-local-part-", 4) + "x@example.com", Name: "Somebody With A Rather Long Display Name Indeed"},
		},
	}
	wishFamily(now).PutIn(d)
	workbenchWorld(d, now)
	stewardStressWorld(d, now)
	return d
}

// commonsLands is the fixture's Lands kill switch: lab, meadow and orchard are tenant lands and orchard is frozen; gardenio
// is the operator's. Set flips it as the Freeze button would.
func commonsLands() commons.LandSwitch {
	var mu sync.Mutex
	frozen := map[string]bool{"lab": false, "meadow": false, "orchard": true}
	return commons.LandSwitch{
		State: func(_ context.Context, land string) (bool, bool) {
			mu.Lock()
			defer mu.Unlock()
			f, tenant := frozen[land]
			return tenant, f
		},
		Set: func(_ *http.Request, land string, f bool, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			frozen[land] = f
			return nil
		},
	}
}

// commonsInvites is the fixture's member invites (Lands step 8.9) for the People lightbox: meadow's are ordinary (the
// commonspeople golden), lab's are the rubric's stress (F1 39-character logins, a list long enough to scroll, one revoking).
// Invite and Revoke change the lists as the viewer would, so a shot can show the answer.
func commonsInvites(now func() time.Time) commons.LandInvites {
	var mu sync.Mutex
	t := now()
	day := 24 * time.Hour
	lists := map[string][]commons.Invite{
		"meadow": {
			{ID: "md-inv3", Login: "hubot", Role: "guest", State: "pending", ExpiresAt: t.Add(12 * day)},
			{ID: "md-inv2", Login: "monalisa", Role: "member", State: "joined", ExpiresAt: t.Add(3 * day)},
			{ID: "md-inv1", Login: "octocat", Role: "member", State: "pending", ExpiresAt: t.Add(2 * day), Revoking: true},
		},
	}
	long := "a-very-long-github-login-of-39-chars-xy"
	for i := 0; i < 14; i++ {
		inv := commons.Invite{ID: "lb-inv" + strconv.Itoa(100+i), Login: long[:39-len(strconv.Itoa(i))] + strconv.Itoa(i),
			Role: []string{"member", "guest"}[i%2], State: []string{"pending", "joined"}[i%3/2], ExpiresAt: t.Add(time.Duration(14-i) * day)}
		inv.Revoking = i == 2
		lists["lab"] = append(lists["lab"], inv)
	}
	return commons.LandInvites{
		List: func(_ context.Context, land string) ([]commons.Invite, error) {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(lists[land]), nil
		},
		Invite: func(_ *http.Request, land, login, role string) (commons.Invite, error) {
			mu.Lock()
			defer mu.Unlock()
			if strings.EqualFold(login, "ghost") {
				return commons.Invite{}, &commons.InviteRefusal{Message: "GitHub has no account called @" + login + "."}
			}
			inv := commons.Invite{ID: land[:2] + "-inv" + strconv.Itoa(len(lists[land])+1), Login: login, Role: role, State: "pending", ExpiresAt: now().Add(14 * day)}
			lists[land] = append([]commons.Invite{inv}, lists[land]...)
			return inv, nil
		},
		Revoke: func(_ *http.Request, land, id string) (commons.Invite, error) {
			mu.Lock()
			defer mu.Unlock()
			for i := range lists[land] {
				if lists[land][i].ID == id {
					lists[land][i].Revoking = true
					return lists[land][i], nil
				}
			}
			return commons.Invite{}, &commons.InviteRefusal{Message: "That invite is no longer open."}
		},
	}
}

// commonsMemberLand is the land the fixture reads as a member, not the operator: orchard, so its frozen banner is the one
// a member sees. Every other request is the operator's.
const commonsMemberLand = "orchard"

// commonsPresence is who the fixture land shows as here (Lands Commons III step 4.3): in #general the viewer, flo and two
// agents, with sam elsewhere in the land; flo in #design; in the lab land's #stress more people than the header shows, with
// the rubric's long and non-Latin names (F1, F4). The viewer is already in every channel a golden shows, so its own stream
// joining there changes nothing on screen and the shots never race it (#escalations was missing, and its first shot drew
// the viewer's M only when the stream beat the header's presence fetch: gd-LZmuRYirV6). Typing (step 4.4): flo is typing in #general, and in
// #stress مريم and the long name are, for a line that has to cut and run right to left; neither runs out during a run.
func commonsPresence() *commons.Presence {
	agents := func(context.Context) (map[string][]commons.Spot, error) {
		general := []commons.Spot{{Land: "gardenio", Channel: "gd-chGeneral"}}
		return map[string][]commons.Spot{
			"agent:gardenio-43-presence-who":                                         general,
			"agent:gardenio-29-the-commons-nec":                                      general,
			"agent:gardenio-crew-" + strings.Repeat("very-long-agent-name-", 4)[:70]: {{Land: "lab", Channel: "lb-chStress"}},
		}, nil
	}
	p := commons.NewPresence(nil, agents, nil)
	p.PollAgents(context.Background())
	for who, at := range map[string]string{"oidc:flo@example.com": "gd-chGeneral", "oidc:sam@example.com": "gd-chCrew"} {
		p.Join(who, commons.Spot{Land: "gardenio", Channel: at})
	}
	p.Join("oidc:flo@example.com", commons.Spot{Land: "gardenio", Channel: "gd-chDesign"})
	for _, at := range []commons.Spot{{Land: "gardenio", Channel: "gd-chGeneral"}, {Land: "gardenio", Channel: "gd-chCrew"}, {Land: "gardenio", Channel: "gd-chDecide"}, {Land: "gardenio", Channel: "gd-chEscal"}, {Land: "gardenio", Channel: "gd-chDesign"}, {Land: "lab", Channel: "lb-chStress"},
		{Land: "meadow", Channel: "md-chGeneral"}, {Land: "orchard", Channel: "or-chGeneral"}} {
		p.Join(commonsViewer, at)
	}
	stress := []string{"oidc:田中花子@example.com", "oidc:مريم@example.com", "oidc:" + strings.Repeat("bartholomew.", 6) + "smith@example.com", "oidc:🧑‍🌾farmer@example.com"}
	for i := 0; i < 12; i++ {
		stress = append(stress, "oidc:person"+strconv.Itoa(10+i)+"@example.com")
	}
	for _, who := range stress {
		p.Join(who, commons.Spot{Land: "lab", Channel: "lb-chStress"})
	}
	p.TypingFor = 24 * time.Hour
	p.Type("oidc:flo@example.com", commons.Spot{Land: "gardenio", Channel: "gd-chGeneral"}, "")
	for _, who := range stress[1:3] {
		p.Type(who, commons.Spot{Land: "lab", Channel: "lb-chStress"}, "")
	}
	return p
}

// snapPath is where the fixture's image attachment (gd-msgSnap) is served, and snapPNG its bytes: a 480x270 farm at dusk,
// drawn here so the goldens get the same pixels everywhere.
const snapPath = "/garden/c/f/gardenio/gd-chGeneral/0123456789abcdef0123456789abcdef/farm-at-dusk.png"

// labSnapPath serves the same picture as the lab #stress land's snapshot (lb-msgSnap), which shows inline (gd-fAgk8mVsta).
const labSnapPath = "/garden/c/f/lab/lb-chStress/0123456789abcdef0123456789abcdef/farm.png"

// labPicPaths serve lb-msgPics' three pictures, labPicPNGs their bytes: the farm at dusk, midnight and dawn.
var labPicPaths = []string{
	"/garden/c/f/lab/lb-chStress/1123456789abcdef0123456789abcdef/palette-dusk.png",
	"/garden/c/f/lab/lb-chStress/2123456789abcdef0123456789abcdef/palette-midnight.png",
	"/garden/c/f/lab/lb-chStress/3123456789abcdef0123456789abcdef/palette-dawn.png",
}
var labPicPNGs = [][]byte{snapPNG, farmPNG(1), farmPNG(2)}

var snapPNG = farmPNG(0)

// farmPNG draws the 480x270 farm: hour 0 at dusk (snapPNG), 1 at midnight, 2 at dawn.
func farmPNG(hour int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 480, 270))
	fill := func(x0, y0, x1, y1 int, c color.RGBA) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(c), image.Point{}, draw.Src)
	}
	for y := 0; y < 120; y++ { // the dusk sky, warm at the horizon
		switch hour {
		case 1: // midnight: deep blue all the way down
			fill(0, y, 480, y+1, color.RGBA{uint8(10 + y/8), uint8(14 + y/6), uint8(48 + y/3), 255})
		case 2: // dawn: pale and pink at the horizon
			fill(0, y, 480, y+1, color.RGBA{uint8(120 + y), uint8(140 + y/2), uint8(200 - y/3), 255})
		default:
			fill(0, y, 480, y+1, color.RGBA{uint8(40 + y), uint8(36 + y/3), uint8(90 - y/4), 255})
		}
	}
	fill(0, 120, 480, 270, [...]color.RGBA{{70, 120, 52, 255}, {28, 52, 30, 255}, {96, 150, 70, 255}}[hour])
	for i, x := range []int{40, 170, 300} { // three plots with rows of crops
		fill(x, 150, x+110, 240, color.RGBA{110, 74, 44, 255})
		for r := 0; r < 4; r++ {
			for c := 0; c < 5; c++ {
				fill(x+10+c*20, 158+r*20, x+20+c*20, 168+r*20, color.RGBA{uint8(150 + 30*i), 200, 80, 255})
			}
		}
	}
	if hour == 2 {
		fill(20, 100, 60, 120, color.RGBA{255, 200, 90, 255}) // the sun, rising
	} else {
		fill(400-hour*180, 40, 430-hour*180, 70, color.RGBA{255, 230, 128, 255}) // the moon
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// memFiles is an in-memory commons.FileStore: the fixture land keeps attachments (gd-S8ufEc3k6s), as beads-viewer does in
// its object store, so the composer offers attaching.
type memFiles struct {
	mu   sync.Mutex
	objs map[string]commons.StoredFile
	body map[string][]byte
}

func (m *memFiles) PutFile(_ context.Context, key, ct string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objs == nil {
		m.objs, m.body = map[string]commons.StoredFile{}, map[string][]byte{}
	}
	m.objs[key], m.body[key] = commons.StoredFile{ContentType: ct}, bytes.Clone(body)
	return nil
}

func (m *memFiles) GetFile(_ context.Context, key string) (commons.StoredFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.objs[key]
	if !ok {
		return commons.StoredFile{}, commons.ErrNoFile
	}
	f.Body = io.NopCloser(bytes.NewReader(m.body[key]))
	return f, nil
}

func (m *memFiles) DeleteFile(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// fixtureFiles holds the fixture land's attachments: the two snapshots and lb-msgPics' three pictures.
func fixtureFiles() *memFiles {
	m := &memFiles{objs: map[string]commons.StoredFile{}, body: map[string][]byte{}}
	put := func(p string, b []byte) {
		m.objs[strings.TrimPrefix(p, "/garden/c/f/")], m.body[strings.TrimPrefix(p, "/garden/c/f/")] = commons.StoredFile{ContentType: "image/png"}, b
	}
	put(snapPath, snapPNG)
	put(labSnapPath, snapPNG)
	for i, p := range labPicPaths {
		put(p, labPicPNGs[i])
	}
	return m
}

// mountCommons serves the fixture land under /garden/c beside the page, reading and writing as commonsViewer with a land
// member's rights, keeping its read positions in d (d.Cursors[commonsViewer]), and opening /garden for a navigation to a
// channel or thread URL (page). hub and presence may be nil.
func mountCommons(mux *http.ServeMux, d *commonstest.Daemon, page *Handler, hub *commons.Hub, presence *commons.Presence) {
	self := func(*http.Request) string { return commonsViewer }
	(&commons.Handler{
		Beads: d, Writes: d, Hub: hub, Nodes: d, Jobs: workbenchJobs(d, d.Now()), Presence: presence, Viewer: self, Cursors: viewerCursors{d}, Files: fixtureFiles(),
		Stewards: wbStewards{d.Now},
		Scope: func(r *http.Request) commons.Scope {
			if strings.HasPrefix(r.URL.Path, "/garden/c/"+commonsMemberLand+"/") {
				return commons.Scope{Lands: []string{"gardenio", "lab", "meadow", commonsMemberLand}, Principal: commonsViewer}
			}
			return commons.Scope{Admin: true, Principal: commonsViewer}
		},
		Lands:       commonsLands(),
		Invites:     commonsInvites(d.Now),
		EmptyPrompt: EmptyPrompt,
		Now:         d.Now,
		React:       fixtureReactor{d},
		Compose:     fixtureComposer{},
		People:      d,
		// the viewer may approve every escalation, so the #escalations shots show Approve and Reject
		Approvals: approveAll{},
		Gate: commons.Gate{
			Self: self,
			// a land member: every card button and a wish head's Done; and, where the fixture reads as the operator (every
			// land but commonsMemberLand), member_manage, the People lightbox
			MayAct: func(_ *http.Request, land, action string) bool {
				return action != commons.ActionMemberManage || land != commonsMemberLand
			},
			May: func(_ *http.Request, ch commons.Channel, _ string) bool {
				return !ch.MembersOnly() || ch.HasMember(commonsViewer)
			},
			MayMembers: func(_ *http.Request, ch commons.Channel, next []string) bool {
				// a member may add or take off only itself
				var added, removed []string
				for _, m := range next {
					if !ch.HasMember(m) {
						added = append(added, m)
					}
				}
				for _, m := range ch.Members {
					if !slices.Contains(next, m) {
						removed = append(removed, m)
					}
				}
				return len(added)+len(removed) == 1 && strings.Join(append(added, removed...), "") == commonsViewer
			},
		},
		Page:   func(w http.ResponseWriter, r *http.Request) bool { page.ServePage(w, r); return true },
		Reason: farmReason(page),
	}).Register(mux)
}

// farmReason is the page's farm's reason for an agent's node head, as beads-viewer wires it (gd-7DyHdnx8fC); nil with no
// farm.
func farmReason(page *Handler) func(string) (string, string) {
	if page == nil || page.Farm == nil {
		return nil
	}
	return page.Farm.GardenerReason
}

// fixtureReactor stands in for the bridge behind a wish head's Done (commons/done.go): it closes the head in the fixture
// daemon, as the bridge's wish close does.
type fixtureReactor struct{ d *commonstest.Daemon }

func (f fixtureReactor) React(ctx context.Context, req commons.ReactRequest) (commons.CardAnswer, error) {
	b, err := f.d.GetBead(beadsapi.WithUserBearer(ctx, commonsViewer), req.Message)
	if err != nil || b == nil {
		return commons.CardAnswer{}, &commons.CardRefusal{Status: http.StatusNotFound, Message: "That wish is gone."}
	}
	closed := *b
	closed.Status = "closed"
	f.d.Put(&closed)
	return commons.CardAnswer{Notes: []string{"Marked done."}}, nil
}

// fixtureComposer stands in for the bridge's mention handling: every mention starts the thread's agent.
// approveAll is a can-approve that says yes to every escalation (escalation routing D9).
type approveAll struct{}

func (approveAll) CanApproveEscalation(context.Context, string) (beadsapi.CanApproveResult, error) {
	return beadsapi.CanApproveResult{Allowed: true, Method: "web_ui", As: "operator"}, nil
}

type fixtureComposer struct{}

func (fixtureComposer) Compose(context.Context, commons.ComposeRequest) (commons.CardAnswer, error) {
	return commons.CardAnswer{Notes: []string{"Starting an agent for this thread."}}, nil
}

// viewerCursors keeps commonsViewer's read positions in the fake daemon, as kbeads keeps them on the viewer's own user bead
// (the fixture's requests carry no bearer of their own).
type viewerCursors struct{ d *commonstest.Daemon }

func (c viewerCursors) ReadCursors(ctx context.Context) (map[string]string, error) {
	return c.d.ReadCursors(beadsapi.WithUserBearer(ctx, commonsViewer))
}

func (c viewerCursors) SetReadCursors(ctx context.Context, cursors map[string]string) (map[string]string, error) {
	return c.d.SetReadCursors(beadsapi.WithUserBearer(ctx, commonsViewer), cursors)
}
