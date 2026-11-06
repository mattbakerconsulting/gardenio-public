package microvm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// This file adds the leaked-resource discovery primitives the C5 lifecycle GC
// drives (epic ka-0000005010, Change C5). Launch/Status/Terminate (backend.go)
// operate on a single known handle; GC needs the inverse — enumerate every
// runtime + per-run secret the launcher owns so the reconciler can cross-check
// them against live agent beads and tear down orphans whose bead is closed or
// gone. A leaked runtime survives a controller crash between RunMicrovm and the
// bead write, or a Terminate that failed after the bead already closed; without
// GC it would burn its full MaximumDurationInSeconds (and the concurrency=1 POC
// budget) before AWS reaps it.

// Runtime is a backend-neutral view of one launched MicroVM as returned by the
// enumeration used for leak GC. BeadID is empty when the runtime carries no
// recoverable bead correlation (for the MicroVM backend the AWS ClientToken IS
// the bead ID, but ListMicrovms does not echo the client token, so the caller
// correlates by MicrovmID against the runtime_id persisted on beads).
type Runtime struct {
	// MicrovmID is the AWS MicroVM identifier — the join key against the
	// runtime_id field the reconciler persisted on the agent bead.
	MicrovmID string

	// State is the raw AWS MicrovmState string.
	State string

	// Live mirrors RuntimeStatus.Live: the runtime is doing (or could resume)
	// work. A leaked runtime is only worth terminating while it is Live — a
	// TERMINATING/TERMINATED one is already going away.
	Live bool

	// StartedAt is when the MicroVM started, used to gate GC on a minimum age so
	// a runtime whose bead-write is merely in flight (spawned seconds ago) is
	// never mistaken for a leak.
	StartedAt time.Time
}

// LeakedSecret is a per-run agent-config secret discovered under the launcher's
// prefix, tagged with its owning bead. GC deletes it when no live bead claims
// the bead ID (the agent finished/was cancelled but Terminate never revoked the
// secret, or a rolled-back launch left it behind).
type LeakedSecret struct {
	// ARN is the secret ARN passed to DeleteSecret.
	ARN string

	// Name is the friendly secret name (…/<bead-id>/agent-config).
	Name string

	// BeadID is the owning agent bead, read from the gardenio:agent-bead tag the
	// backend stamps at CreateSecret. Empty when the tag is absent (a
	// hand-created or legacy secret under the prefix — GC leaves those alone).
	BeadID string

	// CreatedDate is when the secret was created, used to gate GC on a minimum
	// age so a secret written moments before its RunMicrovm is never reaped as a
	// leak mid-launch.
	CreatedDate time.Time
}

// ListRuntimes enumerates every MicroVM the launcher can see, filtered to the
// backend's approved image so GC never touches an unrelated MicroVM sharing the
// launcher account. It paginates ListMicrovms fully. Used by the C5 leak GC to
// find runtimes whose owning bead is gone.
func (b *Backend) ListRuntimes(ctx context.Context) ([]Runtime, error) {
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	var (
		runtimes []Runtime
		token    *string
	)
	for {
		in := &lambdamicrovms.ListMicrovmsInput{NextToken: token}
		// Scope to the approved image so GC only ever considers MicroVMs this
		// backend could have launched — the launcher's RunMicrovm grant is
		// image-scoped, so a leak is always this image.
		if b.cfg.ImageIdentifier != "" {
			in.ImageIdentifier = aws.String(b.cfg.ImageIdentifier)
		}
		out, err := b.microvm.ListMicrovms(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("microvm: ListMicrovms: %w", err)
		}
		for _, item := range out.Items {
			st := statusFromState(item.State, "")
			runtimes = append(runtimes, Runtime{
				MicrovmID: aws.ToString(item.MicrovmId),
				State:     string(item.State),
				Live:      st.Live,
				StartedAt: aws.ToTime(item.StartedAt),
			})
		}
		if out.NextToken == nil || aws.ToString(out.NextToken) == "" {
			break
		}
		token = out.NextToken
	}
	return runtimes, nil
}

// ListPerRunSecrets enumerates every per-run agent-config secret under the
// launcher's prefix, reading the owning bead ID from the gardenio:agent-bead
// tag. It paginates ListSecrets, filtering server-side by the secret-name
// prefix. Used by the C5 leak GC to find secrets whose owning bead is gone (a
// Terminate that never ran, or a launch that rolled back after tagging).
func (b *Backend) ListPerRunSecrets(ctx context.Context) ([]LeakedSecret, error) {
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	var (
		out   []LeakedSecret
		token *string
	)
	for {
		in := &secretsmanager.ListSecretsInput{NextToken: token}
		if b.cfg.SecretPrefix != "" {
			in.Filters = []smtypes.Filter{{
				Key:    smtypes.FilterNameStringTypeName,
				Values: []string{b.cfg.SecretPrefix},
			}}
		}
		resp, err := b.secrets.ListSecrets(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("microvm: ListSecrets: %w", err)
		}
		for _, s := range resp.SecretList {
			name := aws.ToString(s.Name)
			// The name filter is a prefix match; belt-and-braces re-check so an
			// unexpected server-side widening can never delete outside the prefix.
			if b.cfg.SecretPrefix != "" && !strings.HasPrefix(name, b.cfg.SecretPrefix) {
				continue
			}
			out = append(out, LeakedSecret{
				ARN:         aws.ToString(s.ARN),
				Name:        name,
				BeadID:      beadIDFromSecretTags(s.Tags),
				CreatedDate: aws.ToTime(s.CreatedDate),
			})
		}
		if resp.NextToken == nil || aws.ToString(resp.NextToken) == "" {
			break
		}
		token = resp.NextToken
	}
	return out, nil
}

// beadIDFromSecretTags reads the owning bead ID from the gardenio:agent-bead tag
// the backend stamps at CreateSecret. Empty when absent.
func beadIDFromSecretTags(tags []smtypes.Tag) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == tagResourceBead {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// TerminateRuntimeByID terminates a MicroVM by id alone (no per-run secret to
// revoke — the secret leak is swept separately by DeletePerRunSecret). Used by
// leak GC where only the runtime_id survived. Idempotent: a missing MicroVM is
// success. Mirrors Terminate's MicroVM-teardown half.
func (b *Backend) TerminateRuntimeByID(ctx context.Context, microvmID, reason string) error {
	return b.Terminate(ctx, RuntimeHandle{MicrovmID: strings.TrimSpace(microvmID)}, reason)
}

// DeletePerRunSecret force-deletes a leaked per-run secret by ARN. Idempotent
// (already-absent is success). Used by leak GC for a secret whose owning bead
// is gone but whose runtime was already torn down.
func (b *Backend) DeletePerRunSecret(ctx context.Context, arn string) error {
	return b.deleteSecret(ctx, arn)
}

// RevokeIdentityLeases revokes all broker-minted Ziti identity leases for a
// bead — the leak-GC row of the revocation matrix (ka-0000001924), keyed
// purely by bead ID because GC never has a lease id. No-op when the per-run
// issuer is not wired; idempotent (a bead with no leases is a success).
func (b *Backend) RevokeIdentityLeases(ctx context.Context, beadID string) error {
	if b.issuer == nil {
		return nil
	}
	return b.issuer.RevokeLeases(ctx, beadID)
}

// Region reports the AWS region the backend's runtimes live in (us-east-2),
// persisted onto the agent bead as runtime_region.
func (b *Backend) Region() string { return b.cfg.Region }
