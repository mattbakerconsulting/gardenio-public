package commonstest

import (
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Leaks is text in the fixture that a claimlayer reader must never receive:
// the gardenio land's beads, a bead with no project label and one with two.
var Leaks = []string{
	"gd-chgen", "gd-wish1", "gd-msg1", "gd-rep1", "Gardenio secret",
	"nl-msg1", "Unlabelled message", "xx-msg1", "Dual-land message", "nl-chan",
}

// TwoLands is a daemon holding two lands, gardenio and claimlayer, each with a
// #general channel holding a wish head, a message and replies, plus beads that
// belong to no land. ClaimlayerBearer is scoped to claimlayer and
// NoGroupBearer to nothing. Every conversation bead has a created event in the
// log at its creation time. t0 is the time of the first bead.
func TwoLands(t0 time.Time) *Daemon {
	d := &Daemon{Scopes: map[string][]string{ClaimlayerBearer: {"claimlayer"}, NoGroupBearer: {}}}
	// Each land's people. cy is in both lands; dee is gardenio's only.
	d.Members = map[string][]beadsapi.LandMember{
		"claimlayer": {
			{Principal: "oidc:ann@claimlayer.example", Name: "ann"},
			{Principal: "oidc:bo@claimlayer.example", Name: "Bo Lind"},
			{Principal: "oidc:cy@both.example", Name: "cy"},
		},
		"gardenio": {
			{Principal: "oidc:op@gardenio.example", Name: "op"},
			{Principal: "oidc:cy@both.example", Name: "cy"},
			{Principal: "oidc:dee@gardenio.example", Name: "dee"},
		},
	}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	gd, cl := []string{"project:gardenio"}, []string{"project:claimlayer"}
	head := func(land []string) []string { return append([]string{"thread"}, land...) }
	add := func(b *beadsapi.BeadDetail) {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		d.Put(b)
		d.Log("beads.bead.created", b, b.CreatedAt)
	}

	// gardenio
	add(&beadsapi.BeadDetail{ID: "gd-chgen", Type: "channel", Title: "General", Status: "open", Labels: gd, CreatedAt: at(0),
		Fields: map[string]string{"name": "general", "channel_kind": "general", "mirror": "both"}})
	add(&beadsapi.BeadDetail{ID: "gd-wish1", Type: "task", Title: "Gardenio secret wish", Status: "open", Labels: head(gd), CreatedAt: at(1),
		Fields: map[string]string{"channel_id": "gd-chgen"}})
	add(&beadsapi.BeadDetail{ID: "gd-msg1", Type: "message", Title: "Gardenio secret message", Status: "closed", Labels: gd, CreatedAt: at(2),
		Fields: map[string]string{"channel_id": "gd-chgen", "author": "oidc:op@gardenio.example", "text": "Gardenio secret message"}})
	add(&beadsapi.BeadDetail{ID: "gd-rep1", Type: "message", Title: "Gardenio secret reply", Status: "closed", Labels: gd, CreatedAt: at(3),
		Fields: map[string]string{"channel_id": "gd-chgen", "thread_id": "gd-wish1", "author": "agent:gardenio-crew-1"}})

	// claimlayer
	add(&beadsapi.BeadDetail{ID: "cl-chgen", Type: "channel", Title: "General", Description: "Claimlayer chatter", Status: "open", Labels: cl, CreatedAt: at(0),
		Fields: map[string]string{"name": "general", "channel_kind": "general", "mirror": "both"}})
	add(&beadsapi.BeadDetail{ID: "cl-chcrew", Type: "channel", Title: "Crew", Status: "open", Labels: cl, CreatedAt: at(0),
		Fields: map[string]string{"name": "crew", "channel_kind": "crew"}})
	add(&beadsapi.BeadDetail{ID: "cl-charch", Type: "channel", Title: "Old", Status: "closed", Labels: cl, CreatedAt: at(0),
		Fields: map[string]string{"name": "old", "channel_kind": "user"}})
	// A user channel: members-only for posting (spec 2.2.1); ann is in it.
	add(&beadsapi.BeadDetail{ID: "cl-chbuild", Type: "channel", Title: "Build", Status: "open", Labels: cl, CreatedAt: at(0),
		Fields: map[string]string{"name": "build", "channel_kind": "user", "members": `["oidc:ann@claimlayer.example"]`}})
	// A wish from Slack: bound to its Slack thread.
	add(&beadsapi.BeadDetail{ID: "cl-wish1", Type: "task", Title: "Claimlayer wish", Status: "open", Labels: head(cl), CreatedAt: at(1),
		Fields: map[string]string{"channel_id": "cl-chgen", "slack_root_text": "Claimlayer wish: see https://github.com/acme/app/pull/42 and cl-AbCdEfGhIj",
			"slack_thread_channel": "C0CLAIM01", "slack_thread_ts": "1792400000.000100"}})
	add(&beadsapi.BeadDetail{ID: "cl-msg1", Type: "message", Title: "Claimlayer hello", Status: "closed", Labels: cl, CreatedAt: at(2),
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "text": "Claimlayer hello <b>there</b>"}})
	add(&beadsapi.BeadDetail{ID: "cl-rep1", Type: "message", Title: "on it", Status: "closed", Labels: cl, CreatedAt: at(3),
		Fields: map[string]string{"channel_id": "cl-chgen", "thread_id": "cl-wish1", "author": "agent:claimlayer-crew-1", "text": "on it"}})
	// A reply forwarded before channel_id was stamped.
	add(&beadsapi.BeadDetail{ID: "cl-rep2", Type: "message", Title: "legacy reply", Status: "closed", Labels: cl, CreatedAt: at(4),
		Fields: map[string]string{"thread_id": "cl-wish1", "author": "oidc:ann@claimlayer.example", "text": "legacy reply"}})
	// The thread's work, not its conversation.
	add(&beadsapi.BeadDetail{ID: "cl-task1", Type: "task", Title: "Claimlayer member task", Status: "open", Labels: cl, CreatedAt: at(5),
		Fields: map[string]string{"thread_id": "cl-wish1", "channel_id": "cl-chgen"}})
	add(&beadsapi.BeadDetail{ID: "cl-card1", Type: "message", Title: "card cl-agent1", Status: "closed", Labels: cl, CreatedAt: at(6),
		Fields: map[string]string{"channel_id": "cl-chgen", "kind": "card", "subject_id": "cl-agent1", "author": "agent:claimlayer-crew-1", "text": "crew agent"}})
	add(&beadsapi.BeadDetail{ID: "cl-del1", Type: "message", Title: "deleted words", Status: "closed", Labels: cl, CreatedAt: at(7),
		Fields: map[string]string{"channel_id": "cl-chgen", "deleted": "true", "text": "deleted words"}})
	add(&beadsapi.BeadDetail{ID: "cl-crew1", Type: "message", Title: "crew room", Status: "closed", Labels: cl, CreatedAt: at(8),
		Fields: map[string]string{"channel_id": "cl-chcrew", "text": "crew room"}})
	// The agents the cards render, at work (cl-card1) and wrapped up (cl-card2).
	// Agents are not conversation, and cl-card2 is put without a created event,
	// so neither changes what the event log replays.
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "agent", Title: "claimlayer-crew-1", Status: "open", Labels: cl,
		CreatedAt: at(5), UpdatedAt: at(27),
		Fields: map[string]string{"agent": "claimlayer-crew-1", "project": "claimlayer", "role": "crew", "agent_state": "working",
			"task_id": "cl-task1", "live_cost_usd": "1.234", "live_turns": "42", "context_tokens": "123456",
			"operated_by": "oidc:ann@claimlayer.example", "spawned_by": "slack"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent2", Type: "agent", Title: "claimlayer-crew-2", Status: "closed", Labels: cl,
		CreatedAt: at(1), UpdatedAt: at(9),
		Fields: map[string]string{"agent": "claimlayer-crew-2", "project": "claimlayer", "role": "crew", "agent_state": "done",
			"total_cost_usd": "0.42", "total_request_count": "9", "context_tokens": "48000",
			"wrapup": `{"tldr":"Chart lints again","accomplishments":"Fixed the values schema; PR #42 is green","handoff_notes":"Release when convenient"}`}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-card2", Type: "message", Title: "card cl-agent2", Status: "closed", Labels: cl, CreatedAt: at(9),
		Fields: map[string]string{"channel_id": "cl-chcrew", "kind": "card", "subject_id": "cl-agent2", "author": "agent:claimlayer-crew-2", "text": "Agent: claimlayer-crew-2"}})

	// Death and crash (Commons II step 10): an agent that died mid-work, its card
	// in #crew with the crash notice and the death report in the card's thread,
	// and a death-storm roll-up in #crew. Put without created events, like the
	// cards above.
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent3", Type: "agent", Title: "claimlayer-crew-3", Status: "closed", Labels: cl,
		CreatedAt: at(2), UpdatedAt: at(11),
		Fields: map[string]string{"agent": "claimlayer-crew-3", "project": "claimlayer", "role": "crew", "agent_state": "done",
			"close_reason": "context_cancelled_involuntary", "total_cost_usd": "0.80", "total_request_count": "12"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-card3", Type: "message", Title: "card cl-agent3", Status: "closed", Labels: cl, CreatedAt: at(10),
		Fields: map[string]string{"channel_id": "cl-chcrew", "kind": "card", "subject_id": "cl-agent3", "author": "agent:claimlayer-crew-3", "text": "Agent: claimlayer-crew-3"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-crash1", Type: "message", Title: "Agent crashed: claimlayer-crew-3", Status: "closed", Labels: cl, CreatedAt: at(11), CreatedBy: "slack-bridge",
		Fields: map[string]string{"channel_id": "cl-chcrew", "thread_id": "cl-card3", "kind": "notice", "notice_subject": "cl-agent3",
			"text": "Agent crashed: claimlayer-crew-3", "notice": "crash", "notice_agent": "claimlayer-crew-3",
			"notice_pod": "claimlayer-crew-3-pod", "notice_pod_phase": "failed"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-death1", Type: "message", Title: "Agent died: claimlayer-crew-3", Status: "closed", Labels: cl, CreatedAt: at(12),
		Fields: map[string]string{"channel_id": "cl-chcrew", "thread_id": "cl-card3", "kind": "notice", "notice_subject": "cl-agent3",
			"author": "agent:gardenio-forensics", "text": "Agent died: claimlayer-crew-3", "notice": "death", "notice_agent": "claimlayer-crew-3",
			"notice_cause": "the pod was torn down mid-turn; its claimed task went back to the queue"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-storm1", Type: "message", Title: "Death storm: 3 more agents died", Status: "closed", Labels: cl, CreatedAt: at(13),
		Fields: map[string]string{"channel_id": "cl-chcrew", "kind": "notice", "author": "agent:gardenio-forensics",
			"text": "Death storm: 3 more agents died in claimlayer (reports held back)", "notice": "death_storm",
			"notice_count": "3", "notice_window": "5m0s",
			"notice_dead": `[{"id":"cl-agent4","name":"claimlayer-crew-4"},{"id":"cl-agent5","name":"claimlayer-crew-5"}]`}})

	// #decisions: decision and permission cards (Commons II step 3) open,
	// answered and dismissed. Put without created events, like the agents, so
	// the event log replays what it did before.
	d.Put(&beadsapi.BeadDetail{ID: "cl-chdec", Type: "channel", Title: "Decisions", Status: "open", Labels: cl, CreatedAt: at(0),
		Fields: map[string]string{"name": "decisions", "channel_kind": "decisions"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-dec1", Type: "decision", Title: "Ship the chart fix?", Status: "open", Priority: 1, Labels: cl,
		Assignee: "claimlayer-crew-1", CreatedAt: at(10), UpdatedAt: at(10),
		Fields: map[string]string{"prompt": "Ship the chart fix? See cl-AbCdEfGhIj", "context": "The values schema is fixed; CI is green on https://github.com/acme/app/pull/42.",
			"options": `[{"id":"ship","short":"Ship it","label":"Merge and release","description":"Merge PR #42 and cut a release"},{"id":"wait","short":"Wait","artifact_type":"report"}]`}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-dec2", Type: "decision", Title: "Rotate the token now?", Status: "closed", Priority: 2, Labels: cl,
		Assignee: "claimlayer-crew-1", CreatedAt: at(11), UpdatedAt: at(12),
		Fields: map[string]string{"prompt": "Rotate the token now?", "options": `["Now","Tonight"]`, "chosen": "Tonight",
			"rationale": "Fewer people online", "responded_by": "oidc:ann@claimlayer.example"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-dec3", Type: "decision", Title: "Retry the flaky job?", Status: "closed", Priority: 3, Labels: cl,
		CreatedAt: at(12), UpdatedAt: at(13),
		Fields: map[string]string{"prompt": "Retry the flaky job?", "chosen": "dismissed", "canceled": "true", "canceled_by": "oidc:ann@claimlayer.example"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-perm1", Type: "permission", Title: "Permission: Bash for claimlayer-crew-1", Status: "open", Labels: cl, CreatedAt: at(14),
		Fields: map[string]string{"agent": "claimlayer-crew-1", "request_id": "abcde", "tool_name": "Bash", "description": "Run the chart tests",
			"input_preview": "make test-helm && echo '<done>'"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-perm2", Type: "permission", Title: "Permission: Write for claimlayer-crew-1", Status: "closed", Labels: cl, CreatedAt: at(15),
		Fields: map[string]string{"agent": "claimlayer-crew-1", "request_id": "fghij", "tool_name": "Write", "behavior": "deny", "answered_by": "@ann"}})
	for i, c := range [][2]string{{"cl-dcard1", "cl-dec1"}, {"cl-dcard2", "cl-dec2"}, {"cl-dcard3", "cl-dec3"}, {"cl-pcard1", "cl-perm1"}, {"cl-pcard2", "cl-perm2"}} {
		d.Put(&beadsapi.BeadDetail{ID: c[0], Type: "message", Title: "card " + c[1], Status: "closed", Labels: cl, CreatedAt: at(10 + i),
			Fields: map[string]string{"channel_id": "cl-chdec", "kind": "card", "subject_id": c[1], "author": "agent:claimlayer-crew-1", "text": "Decision needed"}})
	}

	// #escalations: escalation cards (Commons II step 4) waiting, approved and
	// running, done (a secret one, so only its redacted summary shows) and
	// rejected. Put without created events, like the decisions.
	d.Put(&beadsapi.BeadDetail{ID: "cl-chesc", Type: "channel", Title: "Escalations", Status: "open", Labels: cl, CreatedAt: at(0),
		Fields: map[string]string{"name": "escalations", "channel_kind": "escalations"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-esc1", Type: "escalation", Title: "Merge PR #42: the chart fix", Status: "open", Priority: 0, Labels: cl, CreatedAt: at(20),
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "execution_target": "local", "sensitivity": "internal",
			"held_by": "operator", "audience": "owner",
			"requesting_agent": "cl-agent1", "requesting_task": "cl-AbCdEfGhIj", "pingback_count": "2", "pingback_reason": "still blocks the release",
			"context": "PR #42 is green and reviewed. Merging ships the values schema fix.", "execution_script": "gh pr merge 42",
			"credential_requirements": `[{"type":"github_token","identifier":"merge"},{"identifier":"kube-prod"}]`,
			"operation_spec":          `{"mr_url":"https://github.com/acme/app/pull/42","mr_iid":42}`}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-esc2", Type: "escalation", Title: "Rotate the deploy key", Status: "open", Priority: 1, Labels: cl, CreatedAt: at(21),
		Fields: map[string]string{"operation_type": "secret_rotate", "execution_target": "server", "requesting_agent": "claimlayer-crew-1",
			"held_by": "server", "audience": "operator",
			"approved_by": "gd-user-1", "approved_by_email": "oidc:ann@claimlayer.example", "approval_method": "web_ui"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-esc3", Type: "escalation", Title: "Read the prod audit log", Status: "closed", Priority: 1, Labels: cl, CreatedAt: at(22),
		Fields: map[string]string{"operation_type": "audit_read", "sensitivity": "secret", "requesting_agent": "claimlayer-crew-1",
			"approved_by_email": "ann@claimlayer.example", "result_status": "success", "executor": "claimlayer-infra-1",
			"redacted_summary": "3 logins from new addresses, all the operator's.", "result": `{"rows":"withheld"}`}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-esc4", Type: "escalation", Title: "Grant cluster-admin to crew", Status: "closed", Priority: 2, Labels: cl, CreatedAt: at(23),
		Fields: map[string]string{"operation_type": "rbac_grant", "requesting_agent": "claimlayer-crew-1",
			"rejected_by": "oidc:ann@claimlayer.example", "rejection_reason": "Too broad: ask for the one namespace."}})
	// Audience (gd-bmOiteQi36): cl-esc1 needs credentials, cl-esc2 is a
	// secret_rotate (a D3 floor operation) and cl-esc3 is secret, so all three
	// are the owner's whatever they say; cl-esc4 defaults to the operators and
	// cl-esc5, a plain merge, to the desk, so the two fold into "In hand: 2
	// (desk 1, operator 1)". The held_by and audience stamps on cl-esc1, cl-esc2
	// and cl-esc5 are what filing writes (escalation routing Task 2; cl-esc2's
	// "server" is Task 4's approve handing it to the executor), so cl-esc1
	// waits on the owner and cl-esc5's chip names coordinator merge-desk; the
	// closed cl-esc3 and cl-esc4 stay unstamped. cl-esc1's asker is
	// its agent bead's id, shown as the agent's name; cl-esc5's names no agent
	// here, so no asker shows.
	d.Put(&beadsapi.BeadDetail{ID: "cl-esc5", Type: "escalation", Title: "Merge PR #43: the docs fix", Status: "open", Priority: 1, Labels: cl, CreatedAt: at(24),
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "audience": "desk", "held_by": "coordinator:merge-desk", "requesting_agent": "gd-Pg1xaiun2c",
			"operation_spec": `{"mr_url":"https://github.com/acme/app/pull/43"}`}})
	for i, c := range [][2]string{{"cl-ecard1", "cl-esc1"}, {"cl-ecard2", "cl-esc2"}, {"cl-ecard3", "cl-esc3"}, {"cl-ecard4", "cl-esc4"}, {"cl-ecard5", "cl-esc5"}} {
		d.Put(&beadsapi.BeadDetail{ID: c[0], Type: "message", Title: "card " + c[1], Status: "closed", Labels: cl, CreatedAt: at(20 + i),
			Fields: map[string]string{"channel_id": "cl-chesc", "kind": "card", "subject_id": c[1], "author": "agent:claimlayer-crew-1", "text": "Escalation request"}})
	}

	// No land: no project label, and two.
	add(&beadsapi.BeadDetail{ID: "nl-chan", Type: "channel", Title: "General", Status: "open", CreatedAt: at(0),
		Fields: map[string]string{"name": "general", "channel_kind": "general"}})
	add(&beadsapi.BeadDetail{ID: "nl-msg1", Type: "message", Title: "Unlabelled message", Status: "closed", CreatedAt: at(9),
		Fields: map[string]string{"channel_id": "cl-chgen", "text": "Unlabelled message"}})
	add(&beadsapi.BeadDetail{ID: "xx-msg1", Type: "message", Title: "Dual-land message", Status: "closed",
		Labels: []string{"project:claimlayer", "project:gardenio"}, CreatedAt: at(10),
		Fields: map[string]string{"channel_id": "cl-chgen", "text": "Dual-land message"}})
	return d
}
