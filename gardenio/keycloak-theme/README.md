# Keycloak login theme (`gardenio`)

The Keycloak theme the gardenio realm uses for its login, account and email
pages: `gardenio/login`, `gardenio/account`, `gardenio/email`, with the Dawn
Meadow pixel-art scene (S1h, gd-0SYmLWCCM5). Keycloak mounts the `gardenio/`
directory at `/opt/keycloak/themes/gardenio`.

## Why this is not in the Helm chart

It used to be `gardenio/helm/gardenio/files/keycloak-theme`, rendered into a
ConfigMap by the chart. Helm stores the whole packed chart (every file,
base64-encoded) **and** the rendered manifest in one Secret,
`sh.helm.release.v1.gardenio.vN`, which Kubernetes caps at 1 MiB. The theme
was in there twice. Release 2026.292.1 failed with

```
Secret "sh.helm.release.v1.gardenio.v186" is invalid: data: Too long: may not be more than 1048576 bytes
```

and every later release would have failed the same way (gd-qiUiXvTX0k). Moving
the theme out took ~200 KB off the record; `scripts/check-helm-release-size.sh`
now fails CI if the record creeps past 900 KiB again.

## How it reaches the pod

1. `gardenio/scripts/keycloak-theme-configmap.py` renders this tree as one
   ConfigMap: each file is a key with `/` encoded as `__`
   (`login__resources__css__gardenio.css`); text in `data`, fonts and raster
   images in `binaryData`. The annotation `gardenio.dev/theme-checksum` is the
   sha256 over the files (sorted `<path> <sha256>` lines).
2. `gardenio/scripts/release-deploy.sh` runs it and `kubectl apply
   --server-side`s the result (`<release>-keycloak-runtime-theme-files` in the
   release namespace) right before `helm upgrade`, after every gate.
3. The chart's Keycloak StatefulSet (`keycloak.runtime.theme.enabled`) mounts
   that ConfigMap, and a `theme-unpack` initContainer copies every key to its
   real path in an emptyDir the keycloak container mounts read-only. An empty
   or missing ConfigMap fails the pod rather than serving the stock theme.
4. The chart reads the checksum annotation off the live ConfigMap (`lookup`)
   into the pod's `checksum/theme` annotation, so a theme edit restarts
   Keycloak (it caches templates in production) and a calver release that
   touches no theme file does not (gd-cW3og8FqhN).

To ship a theme change by hand, with the kubeconfig of the cluster:

```sh
gardenio/scripts/keycloak-theme-configmap.py --release gardenio --namespace gardenio \
  | kubectl apply --server-side --field-manager=release-deploy --force-conflicts -n gardenio -f -
helm upgrade gardenio <packed chart> -n gardenio -f deploy-values/gardenio-k3s.yaml --set global.imageTag=<current tag>
```

The `helm upgrade` is what restarts the pod (it re-reads the checksum); the
apply alone only updates the mounted ConfigMap, which Keycloak will not re-read.

## Tests

- `python3 gardenio/scripts/keycloak-theme-configmap_test.py`: the encoding,
  the checksum, the refusals (a path containing `__`, a non-UTF-8 text file,
  the 900 KiB cap).
- `gardenio/helm/gardenio/tests/keycloak_theme_test.go` (`make test-helm`):
  the StatefulSet contract (names, mounts, initContainer), the generator's
  output against this tree, and the unpack script run for real over the
  generated keys, which must rebuild this tree byte-for-byte.
- `gardenio/tests/keycloak-theme` (`.github/workflows/keycloak-theme.yml`):
  a real Keycloak with this directory mounted, screenshots and passkeys.
- `gardenio/controller/cmd/beads-viewer/theme_test.go`: the viewer's copies of
  the scene assets must match `login/resources` here.

`gardenio/tests/keycloak-theme/gen/index.js` redraws the scene SVGs into
`login/resources/img/` deterministically.
