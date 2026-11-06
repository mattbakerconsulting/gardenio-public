package main

import (
	"context"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/lands/provision"
	"gardenio/controller/internal/lands/watcher"
	"gardenio/controller/internal/mailer"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/steward"
)

// newPurseWatcher builds the Lands purse watcher (internal/lands/watcher,
// Track B spec §3.5, step 8.4): once a minute it parks a tenant land's agents
// at the wall, posts the threshold notices in the land's #general and mails
// the owner, says the steward's empty-purse line for it and parks it (step
// 9.7, steward.Hook), and on a credit grant posts "purse refilled" (the land's
// steward wakes on the grant through its own trigger, internal/steward).
// Operator lands are never touched. pods is how it sees which agents run on a
// land: the pods in the land's namespace, not the agent beads (gd-Nc7yMpW7U6).
//
// Mail comes from the MAILER_* environment (internal/mailer); it is off until
// the operator turns it on, and a mailer that cannot be built is logged and
// left off so the parking still runs. Without the LiteLLM URL and master key
// a frozen land's key is not blocked (the rest of the freeze still holds).
func newPurseWatcher(ctx context.Context, daemon *beadsapi.Client, pods podmanager.Manager, litellmURL, litellmMasterKey string, logger *slog.Logger) *watcher.Watcher {
	logger = logger.With("component", "purse-watcher")
	mailCfg := mailer.ConfigFromEnv()
	m, err := mailer.New(ctx, mailCfg)
	if err != nil {
		logger.Warn("purse watcher: mailer unavailable, purse mail is off", "error", err)
		m = mailer.Disabled{}
	}
	var keys watcher.Keys
	if litellmURL != "" && litellmMasterKey != "" {
		keys = &provision.LiteLLM{BaseURL: litellmURL, MasterKey: litellmMasterKey}
	} else {
		logger.Warn("purse watcher: no LiteLLM URL or master key, a frozen land's key is not blocked")
	}
	return &watcher.Watcher{
		Daemon:    daemon,
		Pods:      pods,
		Keys:      keys,
		Nudger:    purseNudger{daemon: daemon, logger: logger},
		Mailer:    m,
		Templates: mailer.NewTemplates(mailCfg),
		Steward:   steward.Hook{Daemon: daemon},
		Logger:    logger,
	}
}

// purseNudger delivers the watcher's nudges the way the bridge does: an
// interrupt mail the agent's own mail subscriber hands to coop.
type purseNudger struct {
	daemon *beadsapi.Client
	logger *slog.Logger
}

func (n purseNudger) Nudge(ctx context.Context, agentName, message string) error {
	return bridge.NudgeAgent(ctx, n.daemon, n.logger, agentName, message)
}
