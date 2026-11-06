# Creating and rotating Secrets: create or replace, never apply

`kubectl apply` writes the whole object, Secret data included, into the
`kubectl.kubernetes.io/last-applied-configuration` annotation. That copy then
shows up in `kubectl get -o yaml`, backups, and support bundles, and a later
`apply` can quietly restore an old value from it.

| Task | Command |
|---|---|
| New Secret | `kubectl -n NS create secret generic NAME --from-literal=key=VALUE` (or `--from-file=key=PATH`) |
| New Secret from a manifest you generated | `kubectl create secret generic NAME ... --dry-run=client -o yaml \| kubectl -n NS create -f -` |
| Rotate an existing Secret | `kubectl create secret generic NAME ... --dry-run=client -o yaml \| kubectl -n NS replace -f -` |
| Check for leftovers | `scripts/security/check-secret-last-applied.sh` (prints names only) |
| Remove leftovers (operator) | `scripts/security/check-secret-last-applied.sh --strip` |

Helm-managed Secrets are unaffected: Helm does not write this annotation.
CI fails a script or runbook code block that pipes or points `kubectl apply`
at a Secret (`scripts/security/lint-kubectl-apply-secret.py`).
