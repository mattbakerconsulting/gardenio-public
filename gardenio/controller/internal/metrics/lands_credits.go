package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/lands/purse"
)

// Tenant land credit gauges (plan gd-gh5J0vVbJP step 3.2, gd-DxFIK7tips). The
// vmalert rules in helm/gardenio/templates/_lands-credits.tpl turn a land's USD
// bill (gardenio_account_cost_usd{account="lands-<land>"}) into credits with
// these, so every gardenio:land_spend_credits:* series is credits and no tenant
// chart ever has to read a USD figure:
//
//	gardenio_lands_credit_rate{land}      llm_multiplier of the credit rate in
//	                                      force (purse.InForce): credits per US$1.
//	gardenio_lands_credits_granted{land}  the sum of the land's credit_grant
//	                                      ledger (purse.SumCredits).
//
// Both are read the way the purse reads them (internal/lands/purse), so the
// recorded spent and remaining match GET /api/lands/{land}/purse. Only tenant
// lands carry a series; an operator land has no purse and no credit series.
// With no rate in force the rate series is absent, so the credit rules record
// nothing rather than a figure priced at a rate that does not exist.

// landCreditsInterval is how often the ledger is re-read. Grants and rates
// change by hand, a few times a month, and the spend sync that moves the USD
// side runs every 5 minutes, so the 30s scrape loop need not re-read them.
const landCreditsInterval = 5 * time.Minute

func newLandCreditGauges() (rate, granted *prometheus.GaugeVec) {
	rate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "lands_credit_rate",
		Help:      "Credits charged per US$1 of LLM spend for a tenant land: the llm_multiplier of the credit rate in force. Absent when no rate is in force. The gardenio:land_spend_credits:* recording rules multiply the land's USD bill by it.",
	}, []string{"land"})
	granted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "lands_credits_granted",
		Help:      "Credits granted to a tenant land: the sum of its credit_grant ledger, as the purse reads it.",
	}, []string{"land"})
	return rate, granted
}

// collectLandCredits refreshes the land credit gauges at most every
// landCreditsInterval. A failed read keeps the last good values: a partial
// ledger would misstate money, and a stale one is at most one refresh old.
func (c *Collector) collectLandCredits(ctx context.Context) {
	now := time.Now()
	if !c.landCreditsAt.IsZero() && now.Sub(c.landCreditsAt) < landCreditsInterval {
		return
	}
	lands, err := purse.TenantLands(ctx, c.querier)
	if err != nil {
		c.logger.Warn("failed to list tenant lands for credit metrics", "error", err)
		c.scrapeErrors.Inc()
		return
	}
	rates, err := purse.LoadRates(ctx, c.querier)
	if err != nil {
		c.logger.Warn("failed to read the credit rate table for credit metrics", "error", err)
		c.scrapeErrors.Inc()
		return
	}
	granted := make(map[string]float64, len(lands))
	for _, land := range lands {
		grants, err := purse.LoadGrants(ctx, c.querier, land)
		if err != nil {
			c.logger.Warn("failed to read a land's credit ledger for credit metrics", "land", land, "error", err)
			c.scrapeErrors.Inc()
			return
		}
		granted[land] = purse.SumCredits(grants)
	}

	rate := purse.InForce(rates)
	c.landCreditRate.Reset()
	c.landCreditsGranted.Reset()
	for _, land := range lands {
		if rate != nil {
			c.landCreditRate.WithLabelValues(land).Set(rate.LLMMultiplier)
		}
		c.landCreditsGranted.WithLabelValues(land).Set(granted[land])
	}
	c.landCreditsAt = now
}
