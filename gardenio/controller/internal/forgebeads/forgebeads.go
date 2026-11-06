// Package forgebeads holds the bead-graph writes that gitlab-bridge's sync paths
// and github-bridge repeat verbatim. It stays forge-neutral: it knows beads and
// dependency edges, never a GitLab MR or GitHub PR payload. Everything that
// reads a webhook body (state mapping, bead locating by branch or PR text,
// Slack lines) is forge-shaped and deliberately stays in each bridge; the
// bridges mirror each other there and note it where they diverge.
package forgebeads

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"gardenio/controller/internal/beadsapi"
)

// Dep types linking a task bead to the MR bead that carries its code.
const (
	DepProduces    = "produces"     // task -> MR bead
	DepTriggeredBy = "triggered-by" // MR bead -> task (reverse, for MR-rooted queries)
)

// DependencyAdder is the slice of beadsapi.Client LinkTaskToMR needs.
type DependencyAdder interface {
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
}

// BeadGetter is the slice of beadsapi.Client GetBeadIfExists needs.
type BeadGetter interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// LinkTaskToMR writes the produces edge (task -> MR bead) and its reverse
// triggered-by edge. Both are always attempted; the returned error joins any
// failures and each is prefixed with the edge type. The daemon rejects a
// duplicate edge, so a caller replaying an event gets errors on the steady
// state and decides how loudly to log them. actor is the created_by stamped on
// the edges, e.g. "system:gitlab-webhook".
func LinkTaskToMR(ctx context.Context, d DependencyAdder, taskBeadID, mrBeadID, actor string) error {
	var errs []error
	if err := d.AddDependency(ctx, taskBeadID, mrBeadID, DepProduces, actor); err != nil {
		errs = append(errs, fmt.Errorf("%s dep: %w", DepProduces, err))
	}
	if err := d.AddDependency(ctx, mrBeadID, taskBeadID, DepTriggeredBy, actor); err != nil {
		errs = append(errs, fmt.Errorf("%s dep: %w", DepTriggeredBy, err))
	}
	return errors.Join(errs...)
}

// GetBeadIfExists returns (nil, nil) for a bead the daemon does not know, so a
// candidate id scraped from a branch name or PR/MR body can be probed without
// every miss being an error.
func GetBeadIfExists(ctx context.Context, g BeadGetter, id string) (*beadsapi.BeadDetail, error) {
	b, err := g.GetBead(ctx, id)
	if err == nil {
		return b, nil
	}
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	return nil, err
}
