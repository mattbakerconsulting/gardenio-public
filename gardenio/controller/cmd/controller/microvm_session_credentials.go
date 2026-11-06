// MicroVM bead-scoped S3 session credentials (S4 ka-0000006612, epic
// ka-0000007030).
//
// A Lambda MicroVM has no IRSA, and its MINIMAL execution role (S2
// ka-0000005082) grants NO access to gardenio-sessions-prod — so a satellite
// cannot persist/restore its coop session (transcripts, meta, recordings) the
// way a K8s pod does through its ambient ServiceAccount web-identity. This file
// closes that gap: at launch the controller mints a short-lived, BEAD-SCOPED S3
// credential via internal/microvm.SessionVendor (AssumeRoleWithWebIdentity on
// the S2 vendor role + an inline session policy narrowing to
// coop/sessions/<bead-id>/*) and folds it into the runtime-neutral AgentSpec so
// backend.go writes it ONLY into the per-run, task-scoped Secrets Manager
// document — never the run-hook payload, image layers, terraform state, or logs.
//
// In-VM consumption needs no image change: coop's default AWS credential chain
// (coop/crates/cli/src/s3.rs build_aws_client) reads AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN from process env, which the image
// already materializes from the runtime document. COOP_S3_BUCKET / _PREFIX /
// _REGION reach the VM unchanged (they are forwarded from the pod spec's env,
// not stripped by microvmClusterInternalEnvKeys) and COOP_S3_SESSION_KEY is set
// in-VM by `gb init` from the agent bead ID — so the delivered credential and
// the keys coop reads/writes share the same coop/sessions/<bead-id>/ prefix the
// session policy scopes to.
//
// Lifetime + termination: the credential is minted for the vendor role's max
// session duration (12h by default) so a single mint covers the whole VM
// wall-clock lifetime, INCLUDING suspend/resume, with no refresh machinery
// (ka-0000006612 acceptance). STS session credentials cannot be individually
// revoked before expiry, so termination relies on (a) deleting the per-run
// Secrets Manager document — the only place the values live — on every exit
// path (handled already by the C4/C5 backend), and (b) the credential's own TTL.
// The bead-scoped session policy bounds an un-expired copy to the terminating
// bead's own (being-torn-down) session prefix, so there is nothing to add to
// microvmCredentialRevoker.
package main

import (
	"context"
	"log/slog"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
)

// microvmSessionVendorOptions assembles the vendor config from the controller
// config: the S2 vendor role, the sessions bucket/prefix, and the region
// (COOP_S3_REGION, falling back to the MicroVM region so a deployment that only
// pins one still vends in the right region). Returns options whose Enabled()
// reports false when vending is not configured (no role or no bucket).
func microvmSessionVendorOptions(cfg *config.Config) microvm.SessionVendorOptions {
	region := cfg.CoopS3Region
	if region == "" {
		region = cfg.MicrovmRegion
	}
	return microvm.SessionVendorOptions{
		RoleARN:         cfg.MicrovmSessionVendorRoleARN,
		Region:          region,
		BucketName:      cfg.CoopS3Bucket,
		SessionPrefix:   cfg.CoopS3Prefix,
		DurationSeconds: int32(cfg.MicrovmSessionCredentialDurationSeconds),
	}
}

// sessionVendor is the narrow seam the spawn path drives to mint a bead-scoped
// S3 credential. *microvm.SessionVendor satisfies it; tests inject a fake. The
// spawn path passes a nil interface (never a typed-nil *SessionVendor) when
// vending is disabled — handleNonK8sSpawn guards on the concrete pointer before
// wrapping it here.
type sessionVendor interface {
	Vend(ctx context.Context, beadID string) (*microvm.SessionCredentials, error)
	Region() string
}

// resolveMicrovmSessionCredentials mints a bead-scoped S3 session credential for
// a satellite, or returns nil when vending is disabled (vendor nil) or the mint
// fails. Graceful degradation matches the other resolvers in
// microvm_credentials.go: a satellite with no S3 credential still launches — it
// simply cannot persist/restore its coop session — rather than the launch being
// blocked. The credential VALUES are never logged.
func resolveMicrovmSessionCredentials(ctx context.Context, logger *slog.Logger, vendor sessionVendor, beadID string) *microvm.SessionCredentials {
	if vendor == nil {
		return nil
	}
	creds, err := vendor.Vend(ctx, beadID)
	if err != nil {
		logger.Warn("lambda_microvm: failed to mint bead-scoped S3 session credential; satellite will run without session persistence",
			"bead", beadID, "error", err)
		return nil
	}
	logger.Info("lambda_microvm: minted bead-scoped S3 session credential",
		"bead", beadID, "expires_at", creds.Expiration)
	return creds
}

// applyMicrovmSessionCredentials folds a minted S3 session credential into the
// runtime-neutral AgentSpec environment so backend.go writes it ONLY into the
// per-run task-scoped Secrets Manager document. The three AWS_* env vars are the
// standard names coop's default credential chain (and the aws CLI/SDK generally)
// read, so no in-VM image change is needed. COOP_S3_REGION is (re-)stamped from
// the vendor's resolved region so the in-VM coop S3 client targets the region
// the credential + bucket live in even when the pod path left it unset. Nil
// creds set no keys (an absent credential must not look authoritative). Pure and
// side-effect-free for unit testing.
func applyMicrovmSessionCredentials(spec *microvm.AgentSpec, creds *microvm.SessionCredentials, region string) {
	if creds == nil {
		return
	}
	if spec.Environment == nil {
		spec.Environment = map[string]string{}
	}
	spec.Environment["AWS_ACCESS_KEY_ID"] = creds.AccessKeyID
	spec.Environment["AWS_SECRET_ACCESS_KEY"] = creds.SecretAccessKey
	spec.Environment["AWS_SESSION_TOKEN"] = creds.SessionToken
	// Region: only stamp when the vendor resolved one and the pod path did not
	// already forward COOP_S3_REGION — never clobber an explicit value.
	if region != "" {
		if _, ok := spec.Environment["COOP_S3_REGION"]; !ok {
			spec.Environment["COOP_S3_REGION"] = region
		}
	}
}
