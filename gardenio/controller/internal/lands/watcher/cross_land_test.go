package watcher

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/mailer"
)

// The cross-land negative matrix, purse half (Lands Track B step 10.6b,
// gd-cIux5tj6WI; spec §5.6: mail carries nothing of B's to A). Two tenant
// lands: the farm's tomato-patch (A, owner ann) and bravo (B, owner bv). B
// runs low and then hits the wall while A's purse is untouched: every mail
// goes to B's owner and every notice into B's #general, never A's. Then A runs
// low, and only A's owner hears of it. The viewer half is beads-viewer
// TestViewerCrossLandMatrix.
func TestCrossLandMatrix_PurseMail(t *testing.T) {
	f := newFarm(t)
	app := f.d.add("plot_application", "bravo", "open", []string{"project:lands-intake"}, map[string]string{
		"applicant_email": "bv@bravo.example", "github_login": "bv",
	})
	f.d.add("project", "bravo", "open", nil, map[string]string{
		"land_kind": "tenant", "account": "lands-bravo", "land_application_id": app.ID,
	})
	f.d.add("credit_grant", "g", "closed", []string{"project:bravo"}, map[string]string{"land": "bravo", "amount_credits": "100"})
	bAcct := f.d.add("account", "lands-bravo", "open", nil, map[string]string{"total_cost_usd": "0"})
	f.d.add("channel", "general", "open", []string{"project:bravo"}, map[string]string{"channel_kind": "general", "name": "general"})
	f.pods.run("bravo", f.d.add("agent", "bravo-crew-1", "open", []string{"project:bravo"}, map[string]string{
		"agent": "bravo-crew-1", "project": "bravo", "agent_state": "working",
	}))
	bSpend := func(usd float64) {
		bAcct.Fields["total_cost_usd"] = fmt.Sprint(usd)
		bAcct.Fields[purse.FieldSpendSyncedAt] = f.clock.Add(-time.Minute).Format(time.RFC3339)
	}
	bSpend(0)
	f.pass(t)

	bSpend(56) // 84 credits: running low
	f.pass(t)
	bSpend(64) // 96 credits: the wall
	f.pass(t)

	sent := f.mail.Sent()
	if len(sent) != 2 {
		t.Fatalf("control: B's low and empty purse sent %d mails, want 2: %+v", len(sent), sent)
	}
	for _, m := range sent {
		if !slices.Equal(m.To, []string{"bv@bravo.example"}) || strings.Contains(m.Text, "tomato-patch") {
			t.Errorf("B's %s mail went to %v (or names A's land): %.300s", m.Kind, m.To, m.Text)
		}
	}
	for _, b := range f.d.beads {
		if b.Type == "message" && !slices.Equal(b.Labels, []string{"project:bravo"}) {
			t.Errorf("a notice of B's purse was posted with labels %v: %q", b.Labels, b.Fields["text"])
		}
	}
	for _, n := range f.nudger.sent {
		if n.agent != "bravo-crew-1" {
			t.Errorf("B's wall nudged %s, which is not B's agent", n.agent)
		}
	}

	f.spend(56) // now A runs low
	f.pass(t)
	low := f.mail.SentKind(mailer.KindPurseLow)
	if len(low) != 2 || !slices.Equal(low[1].To, []string{"ann@example.com"}) {
		t.Fatalf("A's low purse: want one more purse-low mail, to A's owner only, got %+v", low)
	}
}
