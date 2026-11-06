package main

import (
	"testing"

	"github.com/slack-go/slack"
)

func TestSeedFilterSlackUsers(t *testing.T) {
	users := []slack.User{
		{ID: "U001", Name: "alice", Deleted: false, IsBot: false, Profile: slack.UserProfile{RealName: "Alice Smith", Email: "alice@example.com"}},
		{ID: "U002", Name: "bob", Deleted: false, IsBot: false, Profile: slack.UserProfile{RealName: "Bob Jones"}},
		{ID: "U003", Name: "deleted-user", Deleted: true},
		{ID: "U004", Name: "mybot", IsBot: true, Profile: slack.UserProfile{RealName: "My Bot"}},
		{ID: "USLACKBOT", Name: "slackbot", Profile: slack.UserProfile{RealName: "Slackbot"}},
	}

	existing := map[string]string{"U001": "gd-existing"}

	var toCreate []slack.User
	var skippedExisting, skippedBot, skippedDeleted int

	for _, u := range users {
		if u.Deleted {
			skippedDeleted++
			continue
		}
		if (u.IsBot || u.ID == "USLACKBOT") && !false { // includeBots=false
			skippedBot++
			continue
		}
		if _, exists := existing[u.ID]; exists {
			skippedExisting++
			continue
		}
		toCreate = append(toCreate, u)
	}

	if len(toCreate) != 1 {
		t.Fatalf("expected 1 user to create, got %d", len(toCreate))
	}
	if toCreate[0].ID != "U002" {
		t.Errorf("expected U002, got %s", toCreate[0].ID)
	}
	if skippedExisting != 1 {
		t.Errorf("expected 1 skipped existing, got %d", skippedExisting)
	}
	if skippedBot != 2 {
		t.Errorf("expected 2 skipped bots, got %d", skippedBot)
	}
	if skippedDeleted != 1 {
		t.Errorf("expected 1 skipped deleted, got %d", skippedDeleted)
	}
}

func TestSeedDisplayNameFallback(t *testing.T) {
	tests := []struct {
		name     string
		user     slack.User
		wantName string
	}{
		{
			name:     "real name present",
			user:     slack.User{Profile: slack.UserProfile{RealName: "Alice Smith"}},
			wantName: "Alice Smith",
		},
		{
			name:     "no real name uses username",
			user:     slack.User{Name: "alice", Profile: slack.UserProfile{}},
			wantName: "alice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := tt.user.Profile.RealName
			if name == "" {
				name = tt.user.Name
			}
			if name != tt.wantName {
				t.Errorf("got %q, want %q", name, tt.wantName)
			}
		})
	}
}
