# Coopmux Credential File-Mode & Storage Invariants (W10)

Source: ka-0000006638 (rolled up under ka-0000002746). This document
captures the on-disk and in-memory invariants for the coopmux credential
broker's persisted state (`credentials.json`).

## Invariants

1. **Storage medium is tmpfs by default.** When `coopmux.persistence.enabled`
   is `false` (the default), the credential state directory is backed by a
   K8s `emptyDir` with `medium: Memory` — `credentials.json` lives in RAM
   and never touches a node disk. A `sizeLimit` (default `16Mi`) caps the
   allocation.
2. **File mode 0600.** The encrypted (or, with no key, plaintext) credentials
   file is written with mode `0600` via `set_permissions` on the temp file
   *before* the atomic `rename` swap, so the file is never world- or
   group-readable, even momentarily. Implementation: `coop/crates/mux/src/credential/persist.rs::save_inner`.
3. **Parent directory mode 0700.** The credential state directory is forced
   to `0700` on every persist cycle (K8s `emptyDir` mounts at `0755` by
   default). Implementation: `coop/crates/mux/src/credential/broker.rs::persist`.
4. **At-rest encryption when keyed.** When `COOP_CREDENTIAL_KEY` (or
   `COOP_CREDENTIAL_KEY_FILE`) is set, credentials are AES-256-GCM encrypted
   before being written. W1 (ka-0000000955) made the encryption key mandatory
   in production deploys.
5. **Container hardening.** The coopmux container already runs with
   `runAsNonRoot: true`, `runAsUser: 65534`, `readOnlyRootFilesystem: true`,
   `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`, and
   `capabilities.drop: [ALL]`. There are no remaining write capabilities to
   drop after broker init — `drop: [ALL]` is already the strictest setting
   the Linux capability model allows.
6. **PVC mode is an opt-in trade-off.** Setting
   `coopmux.persistence.enabled: true` switches the volume to a
   `PersistentVolumeClaim` so dynamic-account state survives pod restarts.
   This re-introduces disk-residency for the credential file; enable only
   when cross-restart recovery outweighs the disk-leak risk.

## Where to look

| Concern | File |
|---|---|
| File mode 0600 enforcement | `coop/crates/mux/src/credential/persist.rs` (`save_inner`) |
| Parent dir mode 0700 enforcement | `coop/crates/mux/src/credential/broker.rs` (`persist`) |
| tmpfs / PVC volume selection | `gardenio/helm/gardenio/templates/coopmux/deployment.yaml` |
| Knobs (`persistence.*`) | `gardenio/helm/gardenio/values.yaml` (`coopmux.persistence`) |
| Encryption key handling (W1) | `coop/crates/mux/src/credential/persist.rs` (`resolve_master_key`) |

## Verifying

```sh
# Helm renders an emptyDir(Memory) volume in the default deploy:
helm template gardenio/helm/gardenio --set coopmux.enabled=true | \
  yq 'select(.kind=="Deployment" and .metadata.name|test("coopmux")).spec.template.spec.volumes'

# In a live pod:
kubectl exec -n <ns> <coopmux-pod> -- mount | grep credentials
#   tmpfs on /var/lib/coop/credentials type tmpfs (rw,nosuid,nodev,...)
kubectl exec -n <ns> <coopmux-pod> -- stat -c '%a %n' \
  /var/lib/coop/credentials /var/lib/coop/credentials/credentials.json
#   700 /var/lib/coop/credentials
#   600 /var/lib/coop/credentials/credentials.json
```
