package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/landtoken"
	"gardenio/controller/internal/lands/natsland"
	"gardenio/controller/internal/natsauth"
	"gardenio/controller/internal/podmanager"
)

// landNatsEnabled reports whether tenant pods get their land's own NATS user
// (gd-sS7bSXOdGs): the controller has NATS, the land token key and the auth
// callout's issuer seed. Without all three a tenant pod carries no NATS.
func landNatsEnabled(cfg *config.Config) bool {
	return cfg.NatsURL != "" && cfg.CoopmuxLandTokenKeyFile != "" && cfg.NatsLandCalloutIssuerSeedFile != ""
}

// landNatsTokenMinter mints a land's NATS password (landtoken.MintNATS) at
// its landNatsEpoch under the land token key, read at every mint as
// landCoopmuxTokenMinter does. nil when landNatsEnabled is false.
func landNatsTokenMinter(cfg *config.Config) podmanager.LandTokenMinter {
	if !landNatsEnabled(cfg) {
		return nil
	}
	path := cfg.CoopmuxLandTokenKeyFile
	return func(land string) (string, error) {
		epoch, err := landNatsEpoch(cfg, land)
		if err != nil {
			return "", err
		}
		key, err := landtoken.LoadKey(path)
		if err != nil {
			return "", err
		}
		return landtoken.MintNATS(key, land, epoch)
	}
}

// landNatsEpoch is a tenant land's NATS password epoch, from its project
// bead's natsland.EpochField in the project cache. A land tenantLands does not
// list, or whose epoch does not parse, is an error: it gets no password, and
// the callout refuses it.
func landNatsEpoch(cfg *config.Config, land string) (uint64, error) {
	cfg.ProjectCacheMu.RLock()
	entry, ok := cfg.ProjectCache[land]
	cfg.ProjectCacheMu.RUnlock()
	if !ok || !isTenantLandEntry(land, entry) {
		return 0, fmt.Errorf("%s is not a tenant land", land)
	}
	return natsland.ParseEpoch(entry.NatsEpoch)
}

// fenceTenantNats gives a fenced tenant pod its land's NATS user when
// landNatsEnabled: the land's password from LandNatsTokenSecret, projected
// where the fleet token would be, and coop's NATS env with the land's user
// and subject prefix. Called after fenceTenantEnv and fenceTenantMounts have
// dropped every fleet NATS variable and the fleet token, so COOP_NATS_* is
// only ever the land's. With no relay (COOP_NATS_RELAY) the pod subscribes to
// nothing, which the land user may not do anyway; gb hook-relay's NATS
// fallback publishes hooks.>, which it may not either, so hooks go through
// the daemon alone.
func fenceTenantNats(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	spec.LandNatsToken = landNatsEnabled(cfg)
	if !spec.LandNatsToken {
		return
	}
	spec.NatsTokenSecret = podmanager.LandNatsTokenSecret
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env["COOP_NATS_URL"] = cfg.NatsURL
	spec.Env["COOP_NATS_FORWARD"] = "1"
	spec.Env["COOP_NATS_USER"] = natsland.UserName(spec.Project)
	spec.Env["COOP_NATS_PREFIX"] = natsland.EventPrefix(spec.Project)
	spec.Env["COOP_NATS_TOKEN_FILE"] = podmanager.MountNatsToken + "/" + podmanager.NatsTokenFileName
}

// tenantNatsEnv are the variables fenceTenantNats sets.
var tenantNatsEnv = []string{"COOP_NATS_URL", "COOP_NATS_FORWARD", "COOP_NATS_USER", "COOP_NATS_PREFIX", "COOP_NATS_TOKEN_FILE"}

// startLandNatsCallout answers the NATS server's auth callout for tenant
// lands while ctx lives, when landNatsEnabled. It connects as the
// controller's own NATS user, which the chart lists in auth_users. A land is
// served only while tenantLands lists it, with its password at its
// landNatsEpoch, so a drained land or a bumped epoch loses NATS when the
// land's user expires (natsland.DefaultTTL).
func startLandNatsCallout(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	if !landNatsEnabled(cfg) {
		return
	}
	logger = logger.With("component", "land-nats-callout")
	issuer, err := natsland.LoadIssuer(cfg.NatsLandCalloutIssuerSeedFile)
	if err != nil {
		logger.Error("land nats callout disabled: issuer seed", "error", err)
		return
	}
	opts := append([]nats.Option{
		nats.Name("gardenio-controller-land-nats-callout"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5 * time.Second),
		nats.RetryOnFailedConnect(true),
	}, natsauth.Options(cfg.NatsUser, cfg.NatsToken)...)
	conn, err := nats.Connect(cfg.NatsURL, opts...)
	if err != nil {
		logger.Error("land nats callout disabled: connect", "error", err)
		return
	}
	callout := &natsland.Callout{
		Issuer: issuer,
		Key:    func() ([]byte, error) { return landtoken.LoadKey(cfg.CoopmuxLandTokenKeyFile) },
		Land: func(land string) (uint64, bool) {
			epoch, err := landNatsEpoch(cfg, land)
			if err != nil {
				logger.Debug("land nats callout: no epoch", "land", land, "error", err)
			}
			return epoch, err == nil
		},
		Logger: logger,
	}
	go func() {
		defer conn.Close()
		if err := callout.Start(ctx, conn); err != nil {
			logger.Error("land nats callout stopped", "error", err)
		}
	}()
}
