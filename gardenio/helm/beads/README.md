# Shared beads (Dolt) on the k3s cluster

One `dolt sql-server` for every Gas City city, reachable only over ziti as
`beads.ziti:3306`. Design: `gardenio/plans/shared-beads-on-k3s.md`; operator
doc (connect, backup/restore, cutover, caveats):
`gardenio/docs/beads-on-k3s.md`. Manifests are plain YAML derived from
upstream `gascity/contrib/k8s/` (divergences listed in the doc).

## Install / upgrade (as run on 2026-09-17)

```sh
export KUBECONFIG=~/.kube/gardenio.yaml AWS_PROFILE=gardenio AWS_REGION=us-east-1
cd gardenio/helm/beads
kubectl apply -f namespace.yaml

# Root password: SSM SecureString /gardenio/beads/dolt-root -> Secret dolt-root
kubectl -n beads create secret generic dolt-root \
  --from-literal=password="$(aws ssm get-parameter --name /gardenio/beads/dolt-root \
     --with-decryption --query Parameter.Value --output text)"

# Ziti identity (only with the opt-in ziti.hostBeads; the controller is on Vultr,
# see ../ziti/README.md "Where the ziti controller lives"): mint on the
# controller, enroll on the laptop, keep only the Secret
#   (vultr) ziti edge create identity k3s-beads-host -a beads-hosts -o k3s-beads-host.jwt
#   (laptop) ziti-edge-tunnel enroll -j k3s-beads-host.jwt -i k3s-beads-host.json
kubectl -n beads create secret generic ziti-identity --from-file=k3s-beads-host.json
rm k3s-beads-host.json k3s-beads-host.jwt

kubectl apply -f dolt-configmap.yaml -f dolt-backups-pvc.yaml -f dolt-service.yaml \
  -f dolt-statefulset.yaml -f dolt-backup-cronjob.yaml
# ziti-host moved to the opt-in ../ziti chart (off by default):
#   helm upgrade --install gardenio-ziti ../ziti -n default \
#     --set ziti.enabled=true,ziti.hostBeads.enabled=true
kubectl -n beads get pods,pvc
```

The StatefulSet pins to the node labelled `gardenio.io/lifecycle=on-demand`
(the terraform k3s module's one on-demand server; nodes label themselves from
IMDS at boot). Rotating the password: update the SSM parameter, recreate the
Secret, `kubectl -n beads rollout restart statefulset/dolt` — the init
container re-aligns `root@%` on every start.

Manual backup run: `kubectl -n beads create job --from=cronjob/dolt-backup backup-now`.
