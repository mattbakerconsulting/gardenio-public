package bridge

import (
	"fmt"
	"strings"
	"testing"
)

// Golden-text tests for the format strings touched by gd-J80PXCv7is.
// Each test exercises the modified format string both with beadsViewerURL set
// (asserting the Slack mrkdwn link `<url|label>` appears) and with it empty
// (asserting the plain-text fallback is unchanged).

const testViewerURL = "https://viewer"

// --- bot_decisions.go --------------------------------------------------------

func TestDecisionPredecessorChainLine(t *testing.T) {
	cases := []struct {
		name      string
		viewerURL string
		predID    string
		predTitle string
		want      string
	}{
		{
			name:      "linked when title resolved",
			viewerURL: testViewerURL,
			predID:    "gd-pred01",
			predTitle: "Earlier decision",
			want:      ":link: _Chained from: <https://viewer/gd-pred01|Earlier decision>_",
		},
		{
			name:      "linked when title missing falls back to ID",
			viewerURL: testViewerURL,
			predID:    "gd-pred02",
			predTitle: "",
			want:      ":link: _Chained from: <https://viewer/gd-pred02|gd-pred02>_",
		},
		{
			name:      "plain fallback when viewer URL unset",
			viewerURL: "",
			predID:    "gd-pred03",
			predTitle: "Earlier decision",
			want:      ":link: _Chained from: Earlier decision_",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bot{beadsViewerURL: tc.viewerURL}
			predDisplay := b.beadTitleLink(tc.predID, tc.predTitle)
			got := fmt.Sprintf(":link: _Chained from: %s_", predDisplay)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecisionAgentContextLine(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("Agent: %s | _%s_", "<@U123>", b.beadTitleLink("gd-dec01", "Pick X or Y"))
		want := "Agent: <@U123> | _<https://viewer/gd-dec01|Pick X or Y>_"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("_%s_", b.beadTitleLink("gd-dec01", "Pick X or Y"))
		want := "_Pick X or Y_"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestEscalationHeaderAndFallback(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf(":rotating_light: *ESCALATED: %s*", b.beadTitleLink("gd-esc01", "Prod write"))
		want := ":rotating_light: *ESCALATED: <https://viewer/gd-esc01|Prod write>*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		fallback := fmt.Sprintf("ESCALATED: %s — %s", b.beadTitleLink("gd-esc01", "Prod write"), "Approve?")
		wantFallback := "ESCALATED: <https://viewer/gd-esc01|Prod write> — Approve?"
		if fallback != wantFallback {
			t.Fatalf("got %q, want %q", fallback, wantFallback)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf(":rotating_light: *ESCALATED: %s*", b.beadTitleLink("gd-esc01", "Prod write"))
		want := ":rotating_light: *ESCALATED: Prod write*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// --- bot_decisions_modal.go --------------------------------------------------

func TestDecisionModalOriginalDecisionLine(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("Original decision: %s", b.beadLink("gd-dec01", "`gd-dec01`"))
		want := "Original decision: <https://viewer/gd-dec01|`gd-dec01`>"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("Original decision: %s", b.beadLink("gd-dec01", "`gd-dec01`"))
		want := "Original decision: `gd-dec01`"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestDecisionModalPromotedToEscalation(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf(":rotating_light: *Promoted to Escalation*\n\nThis decision has been promoted to escalation %s.\nOperation type: *%s* | Target: *%s*\nPromoted by %s",
			b.beadLink("gd-esc99", "`gd-esc99`"), "write", "prod", "<@U1>")
		if !strings.Contains(got, "<https://viewer/gd-esc99|`gd-esc99`>") {
			t.Fatalf("expected promoted-link in %q", got)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("escalation %s", b.beadLink("gd-esc99", "`gd-esc99`"))
		if got != "escalation `gd-esc99`" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestDecisionModalSuperseded(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("*Superseded*\n\nA follow-up decision (%s) has been posted in this thread.", b.beadLink("gd-new01", "`gd-new01`"))
		if !strings.Contains(got, "<https://viewer/gd-new01|`gd-new01`>") {
			t.Fatalf("expected superseded link in %q", got)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("(%s)", b.beadLink("gd-new01", "`gd-new01`"))
		if got != "(`gd-new01`)" {
			t.Fatalf("got %q", got)
		}
	})
}

// --- bot_escalations.go ------------------------------------------------------

func TestEscalationApprovedHeader(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf(":white_check_mark: *Escalation Approved*\n*%s*", b.beadTitleLink("gd-esc02", "Drop table"))
		want := ":white_check_mark: *Escalation Approved*\n*<https://viewer/gd-esc02|Drop table>*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("*%s*", b.beadTitleLink("gd-esc02", "Drop table"))
		if got != "*Drop table*" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestEscalationCompleteHeader(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("%s *Escalation Complete*: %s\n*%s*", ":white_check_mark:", "succeeded", b.beadTitleLink("gd-esc03", "Run migration"))
		if !strings.Contains(got, "<https://viewer/gd-esc03|Run migration>") {
			t.Fatalf("expected link, got %q", got)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("*%s*", b.beadTitleLink("gd-esc03", "Run migration"))
		if got != "*Run migration*" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestEscalationRejected(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf(":no_entry: *Escalation Rejected*\n%s", b.beadLink("gd-esc04", "`gd-esc04`"))
		want := ":no_entry: *Escalation Rejected*\n<https://viewer/gd-esc04|`gd-esc04`>"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf(":no_entry: *Escalation Rejected*\n%s", b.beadLink("gd-esc04", "`gd-esc04`"))
		want := ":no_entry: *Escalation Rejected*\n`gd-esc04`"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestEscalationRequestHeader(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("\n*%s*", b.beadTitleLink("gd-esc05", "Reset DB"))
		want := "\n*<https://viewer/gd-esc05|Reset DB>*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("\n*%s*", b.beadTitleLink("gd-esc05", "Reset DB"))
		want := "\n*Reset DB*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// --- bot_agents_wrapup.go ----------------------------------------------------

func TestFormatWrapUpSlackBeadsClosedLinking(t *testing.T) {
	wrapJSON := `{"accomplishments":"shipped X","beads_closed":["gd-aaa","gd-bbb"]}`

	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := b.formatWrapUpSlack(Garden, wrapJSON)
		if !strings.Contains(got, "<https://viewer/gd-aaa|gd-aaa>") {
			t.Errorf("missing gd-aaa link in %q", got)
		}
		if !strings.Contains(got, "<https://viewer/gd-bbb|gd-bbb>") {
			t.Errorf("missing gd-bbb link in %q", got)
		}
		// Linked list joined with comma+space.
		if !strings.Contains(got, "*Beads closed:* <https://viewer/gd-aaa|gd-aaa>, <https://viewer/gd-bbb|gd-bbb>") {
			t.Errorf("expected joined linked list, got %q", got)
		}
	})

	t.Run("plain fallback unchanged", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := b.formatWrapUpSlack(Garden, wrapJSON)
		want := "*Beads closed:* gd-aaa, gd-bbb"
		if !strings.Contains(got, want) {
			t.Errorf("expected plain list %q in %q", want, got)
		}
	})
}

// --- bot_agent_kill.go -------------------------------------------------------

func TestAgentKillResumingStatus(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink("gd-trk01", "gd-trk01"))
		want := "Resuming... (tracking: <https://viewer/gd-trk01|gd-trk01>)"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink("gd-trk01", "gd-trk01"))
		want := "Resuming... (tracking: gd-trk01)"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestAgentKillRestartMessage(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf(":arrows_counterclockwise: Agent *%s* restarted with session resume. (tracking: %s)",
			"alice", b.beadLink("gd-trk02", "`gd-trk02`"))
		if !strings.Contains(got, "<https://viewer/gd-trk02|`gd-trk02`>") {
			t.Fatalf("expected link, got %q", got)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("(tracking: %s)", b.beadLink("gd-trk02", "`gd-trk02`"))
		want := "(tracking: `gd-trk02`)"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// --- bot_mentions.go ---------------------------------------------------------

func TestMentionsTrackingStatusText(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
	}{
		{"Starting up", "Starting up..."},
		{"Forwarded", "Forwarded"},
		{"Spinning up", "Spinning up..."},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/with viewer URL", func(t *testing.T) {
			b := &Bot{beadsViewerURL: testViewerURL}
			got := fmt.Sprintf("%s (tracking: %s)", tc.prefix, b.beadLink("gd-mnt01", "gd-mnt01"))
			want := tc.prefix + " (tracking: <https://viewer/gd-mnt01|gd-mnt01>)"
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
		t.Run(tc.name+"/plain fallback", func(t *testing.T) {
			b := &Bot{beadsViewerURL: ""}
			got := fmt.Sprintf("%s (tracking: %s)", tc.prefix, b.beadLink("gd-mnt01", "gd-mnt01"))
			want := tc.prefix + " (tracking: gd-mnt01)"
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestMentionsNudgeFailureMessage(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf(":warning: Created task for %s but nudge failed (tracking: %s). The agent will pick it up on its next cycle.",
			"<agent-link>", b.beadLink("gd-mnt02", "`gd-mnt02`"))
		if !strings.Contains(got, "<https://viewer/gd-mnt02|`gd-mnt02`>") {
			t.Fatalf("expected link, got %q", got)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("(tracking: %s)", b.beadLink("gd-mnt02", "`gd-mnt02`"))
		if got != "(tracking: `gd-mnt02`)" {
			t.Fatalf("got %q", got)
		}
	})
}

// --- bot_trigger_cmd.go ------------------------------------------------------

func TestTriggerStatusLineLink(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("%s %s *%s*", ":green_circle:", b.beadLink("gd-trg01", fmt.Sprintf("`%s`", "gd-trg01")), "Nightly sweep")
		want := ":green_circle: <https://viewer/gd-trg01|`gd-trg01`> *Nightly sweep*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("%s %s *%s*", ":green_circle:", b.beadLink("gd-trg01", fmt.Sprintf("`%s`", "gd-trg01")), "Nightly sweep")
		want := ":green_circle: `gd-trg01` *Nightly sweep*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// --- bot_schedule_cmd.go -----------------------------------------------------

func TestScheduleStatusLineLink(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("%s %s *%s*", ":green_circle:", b.beadLink("gd-sch01", fmt.Sprintf("`%s`", "gd-sch01")), "Daily release gardener")
		want := ":green_circle: <https://viewer/gd-sch01|`gd-sch01`> *Daily release gardener*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("%s %s *%s*", ":green_circle:", b.beadLink("gd-sch01", fmt.Sprintf("`%s`", "gd-sch01")), "Daily release gardener")
		want := ":green_circle: `gd-sch01` *Daily release gardener*"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// --- bot_commands.go ---------------------------------------------------------

func TestCommandsBeadFooterLink(t *testing.T) {
	t.Run("with viewer URL", func(t *testing.T) {
		b := &Bot{beadsViewerURL: testViewerURL}
		got := fmt.Sprintf("\nBead: %s · Use `/roster` to check status.", b.beadLink("gd-cmd01", "`gd-cmd01`"))
		want := "\nBead: <https://viewer/gd-cmd01|`gd-cmd01`> · Use `/roster` to check status."
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("plain fallback", func(t *testing.T) {
		b := &Bot{beadsViewerURL: ""}
		got := fmt.Sprintf("\nBead: %s · Use `/roster` to check status.", b.beadLink("gd-cmd01", "`gd-cmd01`"))
		want := "\nBead: `gd-cmd01` · Use `/roster` to check status."
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}
