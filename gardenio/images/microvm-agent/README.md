# microvm-agent — Lambda MicroVM ARM64 agent image

Gardenio-owned build of the minimum ARM64 Gardenio agent that runs inside an
AWS Lambda MicroVM and reaches internal Gardenio services over an in-VM
OpenZiti userspace proxy. This is the productionized home of the image proven
by the PoC epic (ka-0000005010, 7/7 live E2E) and previously owned by
network-fabric-deployment (nfd!15). Task of record: ka-0000001041 under epic
ka-0000003545.

## Parity contract (Phase 1)

The image preserves the proven NF behavior byte-for-byte where practical:

- `Dockerfile`, `entrypoint.py`, `update-ca-certificates`, `.dockerignore`,
  and `coop-al2023.Dockerfile` started as **byte-for-byte copies** of the
  nfd!15 sources (`infra/overlay/ziti-microvm/image/`, `.../build/`).
  First deliberate divergence: ka-0000007184 added the slack-bridge proxy
  listener (below) so in-VM `gb slack` works.
  Second deliberate divergence: ka-0000005494 taught the entrypoint to consume
  the ziti-access-broker Option-A per-run secret
  (`{"type":"ziti_enrollment_jwt","jwt":…}`) by running `ziti edge enroll`
  in-VM — the broker rejects `enroll:true`, so images that only accept
  pre-enrolled identity JSON 503 the run hook and every per-run-identity
  launch dies. The enrolled-identity JSON shape is still accepted (static POC
  secret back-compat); the resume hook reuses the already-enrolled identity
  instead of re-consuming the one-time JWT.
- The entrypoint contract is unchanged: lifecycle HTTP server on port 8080
  serving `/aws/lambda-microvms/runtime/v1/{ready,validate,run,resume,suspend,terminate}`
  and `GET /health`.
- Ziti proxy listeners: beads `127.0.0.1:18080`, coopmux `127.0.0.1:19800`,
  NATS `127.0.0.1:14222`, LiteLLM `127.0.0.1:14000`, slack-bridge
  `127.0.0.1:18090` (ka-0000007184 — requires the `gardenio-prod-slackbridge`
  Ziti service on the NF side; the entrypoint stamps `SLACK_BRIDGE_URL` only
  when the listener is present in `ZITI_PROXY_SERVICES`), beads-viewer
  `127.0.0.1:18095` (ka-0000001256 — requires `#gardenio-microvm-agents` in
  the `gardenio-prod-beads-viewer` Ziti service's dial roles on the NF side;
  the entrypoint stamps `BEADS_VIEWER_INTERNAL_URL` only when the listener
  is present).
- Run-hook payload / Secrets Manager materialization is unchanged
  (`identitySecretArn`, `agentSecretArn`, `mode`, `runAgent`; secrets land in
  `/home/agent/.runtime-secrets`, `~/.claude/.credentials.json`, `~/.ssh`).

What changed vs. NF is only *where the inputs come from*:

- `build-rootfs.sh` replaces NF's `bin/build-gardenio-microvm-artifacts.sh`.
  It assembles the identical `artifacts/` layout from **this** repo (no
  separate Gardenio checkout) with gb/kd/coop provided as prebuilt linux/arm64
  binaries and the Claude Code tarballs pre-downloaded + sha512-verified.
- Version/checksum pins come from the repo's single source of truth,
  `gardenio/images/agent/install/{versions.env,checksums.env}` (the Claude
  Code tarball sha512s were added there).
- `smoke-test.sh` is a port of NF's `bin/test-gardenio-microvm-image.sh` with
  the same assertions.

## CI pipeline

No workflow builds or publishes this image today: images.yml does not list it
(see `gardenio/docs/deploy-inventory.md`). Until RWX was deleted on 2026-09-16,
an RWX pipeline on native arm64 agents built, smoke-tested and published it on
these triggers:

| Ref | Tags pushed |
|---|---|
| MR / PR | build + smoke only (status check `MicroVM Image`), no push |
| main push | `main-<sha7>` + `latest` |
| tag push (`20*`) | `<calver>-rc`, promoted to `<calver>` + `latest` by `promote.yml` after the E2E gate |

Two artifacts are published to `<registry>/gardenio`:

- **`microvm-agent`** — the runnable image, built with the local-test base
  (`public.ecr.aws/lambda/microvms:al2023-minimal`). This is what CI
  smoke-tests; it is the parity/QA proxy for what AWS builds server-side.
- **`microvm-agent-context`** — a dist-only artifact image carrying
  `/context/context.zip`: the exact build context (Dockerfile +
  `artifacts/`) that the AWS Lambda MicroVM image build consumes. This is
  the **versioned artifact the Gardenio composition registers from** (S6
  ka-0000008012): `gardenio/scripts/microvm/publish-microvm-context.sh <calver>`
  extracts this zip and uploads it to the Gardenio-account artifact bucket
  (`acme-gardenio-prod-microvm-agent-123456789012-us-east-1/microvm-images/<calver>/context.zip`),
  which `awscc_lambda_microvm_image.gardenio_microvm_agent` in
  `composition/canonical-prod` registers as
  `arn:aws:lambda:us-east-1:123456789012:microvm-image:gardenio-prod-microvm-agent`.
  Network Fabric no longer owns registration. Extract the zip manually with:

  ```sh
  crane export <registry>/gardenio/microvm-agent-context:<calver> - \
    | tar -xO context/context.zip > context.zip
  ```

Both images carry `org.opencontainers.image.revision=<commit>` and are
covered by `promote.yml`'s presence + build-provenance gates.

## Local build

```sh
# 1. Build the inputs (from repo root; needs docker with arm64 support):
docker buildx build --platform linux/arm64 \
  --file gardenio/images/microvm-agent/coop-al2023.Dockerfile \
  --output type=local,dest=/tmp/coop-out coop
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
  -o /tmp/gb ./gardenio/controller/cmd/gb
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
  -o /tmp/kd ./kbeads/cmd/kd
source gardenio/images/agent/install/versions.env
curl -fsSL -o /tmp/cc.tgz "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-${CLAUDE_CODE_VERSION}.tgz"
curl -fsSL -o /tmp/cc-arm64.tgz "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-${CLAUDE_CODE_VERSION}.tgz"

# 2. Assemble the build context:
GB_BIN=/tmp/gb KD_BIN=/tmp/kd COOP_BIN=/tmp/coop-out/coop \
  CLAUDE_TGZ=/tmp/cc.tgz CLAUDE_ARM64_TGZ=/tmp/cc-arm64.tgz \
  gardenio/images/microvm-agent/build-rootfs.sh

# 3. Build + smoke test:
docker buildx build --platform linux/arm64 --load \
  -t gardenio-lambda-microvm:local gardenio/images/microvm-agent
gardenio/images/microvm-agent/smoke-test.sh gardenio-lambda-microvm:local
```

`artifacts/` is generated output — never commit it.
