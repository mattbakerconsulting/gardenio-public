# Tarboat — Local-CA TLS for a true air-gap box

> **Tarboat T1 / G1.4 (ka-0000003591), epic ka-0000002483.** Operator: Matt Baker.
> Operator decision Q2 = **true air-gap** (no inbound/outbound internet at install
> or runtime). This is the TLS half of that profile — the part `TARBOAT-SETUP.md`
> §2 and the validation harness assume.

## Why local-CA (and not sslip.io + Let's Encrypt)

The gardenio-in-a-box default is sslip.io hostnames + Let's Encrypt **HTTP-01**,
which mints *publicly-trusted* certs with no DNS records. But HTTP-01 needs Let's
Encrypt to reach the box on `:80`, and sslip.io resolution needs outbound DNS — so
it is **fundamentally an internet-connected mode**. A truly air-gapped box has
neither, so it must be its own certificate authority.

The old escape was "pre-create a static cert Secret by hand and point
`tls.existingSecretName` at it" — manual, undocumented, and easy to get wrong.
G1.4 replaces that with a **chart-minted local-CA chain**: cert-manager builds the
entire trust chain in-cluster, with zero internet.

## What the chart renders in `tls.mode: localCA`

`templates/tls-clusterissuer.yaml` emits a three-link chain (gated on
`tls.mode == localCA`):

```
SelfSigned ClusterIssuer  →  CA Certificate (isCA, 10y, ECDSA)  →  CA ClusterIssuer
  <fullname>-selfsigned        secret <fullname>-local-ca           name = tls.issuer
```

and `templates/tls.yaml` issues the **leaf** Certificate from that CA
ClusterIssuer, enumerating every `tls.hosts` entry into one shared secret (so the
single served cert satisfies `sniStrict`, exactly like `perHost`).

No ACME server, no DNS solver, no DNS-provider credential, no internet. The only
prerequisite is **cert-manager's CRDs + controller**, which `bootstrap-ec2.sh`
stage 4c stages offline from the tarball (G1.1) — so the whole chain comes up
air-gapped.

## How to enable it

Layer the localCA block over the in-box overlay (see the `tls:` section of
`gardenio/helm/gardenio/values-inbox.yaml` for the canonical snippet):

```yaml
tls:
  enabled: true
  mode: localCA
  issuer: gardenio-local-ca-issuer   # MUST match the CA ClusterIssuer name the chart renders
  clusterIssuer:
    create: false                   # do NOT also render the ACME/HTTP-01 issuer
  hosts:                            # every served hostname; one cert covers all SNI
    - "auth.<host>.box.local"
    - "beads-viewer.<host>.box.local"
    - "coopmux.<host>.box.local"
    - "grafana.<host>.box.local"
    - "<host>.box.local"            # apex — ForwardAuth callback host
```

`<host>` is whatever stable name the box answers to on the air-gapped network
(a local-DNS A record, or a dashed-IP `.box.local`-style name the operator
controls). The hosts list must match the IngressRoute hosts the chart serves.

## The one-time client trust import (the cost of local-CA)

Because the CA is self-signed, clients will reject the leaf cert until they trust
the CA. Export the CA once:

```sh
# On the box (kubectl over the SSH tunnel — the API is not public):
kubectl -n gardenio get secret gardenio-local-ca \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > gardenio-local-ca.crt
```

Distribute `gardenio-local-ca.crt` to each operator/client and import it:

**Linux (system trust, covers curl/git/most CLIs):**
```sh
sudo cp gardenio-local-ca.crt /usr/local/share/ca-certificates/gardenio-local-ca.crt
sudo update-ca-certificates
```

**macOS (system keychain):**
```sh
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain gardenio-local-ca.crt
```

**Firefox** (has its own store): Settings → Privacy & Security → Certificates →
View Certificates → Authorities → Import → select the `.crt` → trust for websites.

**Chrome/Edge** use the OS store on macOS/Windows; on Linux import into the NSS db:
```sh
certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n "Gardenio Local CA" -i gardenio-local-ca.crt
```

**iOS:** AirDrop/email the `.crt` → Settings → General → VPN & Device Management →
install the profile → then Settings → General → About → Certificate Trust Settings
→ enable full trust for the Gardenio Local CA.

**`gb`/`kd` CLI / agents on the box:** the in-cluster components already trust the
chain (it's signed by the in-cluster CA and the kube-internal CA bundle covers
service-to-service); only *external* clients reaching the box need the import.

The CA is minted with a **10-year** lifetime and `rotationPolicy: Never`, so this
import is a one-time action per client — rotating the CA would invalidate every
client's trust, so it is deliberately long-lived.

## Verifying

```sh
# From a client that imported the CA — no -k, verify must succeed:
curl -sS https://beads-viewer.<host>.box.local/ -o /dev/null \
  -w '%{http_code} verify=%{ssl_verify_result}\n'   # verify=0 = trusted by the imported CA
```

A `302` to the Keycloak login proves the route is auth-gated; `verify=0` proves the
imported CA validates the chart-minted leaf. This is the air-gap analog of the
sslip.io acceptance probe in `gardenio-in-a-box-install.md` Part B.2.

Operator: Matt Baker
