# LiteLLM AWS POC Smoke Runbook

Verification steps for the bundled `gardenio-litellm` proxy on the AWS POC cluster
(`poc-gardenio-eks`, account `123456789012`, `us-east-1`). Run these after every
chart roll, terraform apply, or NAT GW / VPC change.

For the production cutover plan, see
[`litellm-production-cutover.md`](./litellm-production-cutover.md).

## Prerequisites

- AWS creds for the customer-poc POC account, materialized from
  `gardenio/customer-poc-aws-creds` (see advice `ka-0000007102` /
  `ka-0000007250`):

  ```bash
  CREDS=$(aws secretsmanager get-secret-value \
    --secret-id gardenio/customer-poc-aws-creds \
    --region us-east-1 \
    --query SecretString --output text)
  export AWS_ACCESS_KEY_ID=$(echo "$CREDS" | jq -r .AWS_ACCESS_KEY_ID)
  export AWS_SECRET_ACCESS_KEY=$(echo "$CREDS" | jq -r .AWS_SECRET_ACCESS_KEY)
  unset AWS_SESSION_TOKEN AWS_ROLE_ARN AWS_WEB_IDENTITY_TOKEN_FILE
  aws eks update-kubeconfig --name poc-gardenio-eks --region us-east-1
  ```

- The litellm pod uses a minimal upstream image — no `curl`, no `getent`. Use
  `python3` from the `litellm` container for in-pod network probes.

Discover the running pod once and reuse:

```bash
POD=$(kubectl -n gardenio-system get pod \
  -l app.kubernetes.io/component=litellm \
  -o jsonpath='{.items[0].metadata.name}')
```

## 1. Pod health

```bash
kubectl -n gardenio-system get pods -l app.kubernetes.io/component=litellm
# Expect: 1/1 Running, 0 restarts

kubectl -n gardenio-system exec "$POD" -c litellm -- python3 -c '
import urllib.request
with urllib.request.urlopen("http://localhost:4000/health/readiness", timeout=5) as r:
    print(r.status, r.read(120))
'
# Expect: 200 {"status":"connected","db":"connected",...}
```

## 2. Egress to api.anthropic.com via NAT GW

The POC VPC routes pod egress through three NAT Gateways (tier b output). This
check confirms the litellm pod can resolve, TCP-connect, and TLS-handshake to
`api.anthropic.com` over the public path, and that the egress source IP matches
one of the NAT GW EIPs.

```bash
kubectl -n gardenio-system exec "$POD" -c litellm -- python3 -c '
import socket, urllib.request, json, time

addrs = socket.getaddrinfo("api.anthropic.com", 443, socket.AF_INET, socket.SOCK_STREAM)
print("dns:", [a[4][0] for a in addrs[:3]])

t0 = time.time()
s = socket.create_connection(("api.anthropic.com", 443), timeout=10)
print(f"tcp_ok peer={s.getpeername()} in {(time.time()-t0)*1000:.0f}ms")
s.close()

t0 = time.time()
try:
    req = urllib.request.Request("https://api.anthropic.com/v1/messages",
        headers={"anthropic-version": "2023-06-01"})
    urllib.request.urlopen(req, timeout=10).read()
except urllib.error.HTTPError as e:
    print(f"https_ok http={e.code} in {(time.time()-t0)*1000:.0f}ms")

with urllib.request.urlopen("https://api.ipify.org?format=json", timeout=10) as r:
    print("egress_ip:", json.loads(r.read()))
'
```

Expected:

- `dns:` a public Anthropic IPv4 (e.g. `160.79.104.10`).
- `tcp_ok` and `https_ok http=405` (or 401) — TLS handshake completed; Anthropic
  rejects the GET with a clean JSON error.
- `egress_ip:` matches one of the POC NAT Gateway EIPs:

  ```bash
  aws ec2 describe-nat-gateways --region us-east-1 \
    --filter "Name=state,Values=available" \
    --query 'NatGateways[].NatGatewayAddresses[0].PublicIp' --output text
  ```

If the egress IP does **not** match a NAT GW EIP, the pod is taking an
unexpected path — investigate the EKS node's subnet route table before
proceeding.

## 3. OAuth Bearer forwarding (cluster-internal)

The chart sets `forward_client_headers_to_llm_api: true` (see
`gardenio/helm/gardenio/values.yaml` and the rendered configmap
`gardenio-litellm-config`). Agent pods send
`Authorization: Bearer <OAuth token>` and litellm relays it untouched to
`api.anthropic.com`. AVA edge auth only fronts the controller and viewer;
the agent→litellm hop is cluster-internal, so AVA does not see it.

This probe exercises the relay from inside the cluster using a deliberately
invalid OAuth token. Anthropic should reject it with a structured
`authentication_error` that contains a real `request_id` — that response is
the proof the header survived the hop.

```bash
MASTER=$(kubectl -n gardenio-system get secret gardenio-litellm-master-key \
  -o jsonpath='{.data.master-key}' | base64 -d)

kubectl -n gardenio-system exec "$POD" -c litellm -- python3 -c "
import urllib.request, json, time
body = json.dumps({
    'model': 'claude-opus-4-7-oauth',
    'max_tokens': 8,
    'messages': [{'role':'user','content':'ping'}],
}).encode()
req = urllib.request.Request(
    'http://gardenio-litellm.gardenio-system.svc.cluster.local:4000/v1/messages',
    data=body, method='POST',
    headers={
        'Authorization': 'Bearer sk-ant-oat01-fake-test-token',
        'anthropic-beta': 'oauth-2025-04-20',
        'anthropic-version': '2023-06-01',
        'x-litellm-api-key': '${MASTER}',
        'content-type': 'application/json',
    })
try:
    urllib.request.urlopen(req, timeout=15).read()
except urllib.error.HTTPError as e:
    print('http:', e.code); print('body:', e.read(600))
"
```

Expected: HTTP 401 with a body containing

```
"type":"authentication_error","message":"Invalid bearer token"
... "request_id":"req_..."
```

Pass criteria:

- **`Invalid bearer token`** — Anthropic recognized the OAuth scheme, which
  means both `Authorization: Bearer` and `anthropic-beta: oauth-2025-04-20`
  were forwarded.
- **`request_id` is present** — the request reached Anthropic (litellm did not
  short-circuit).

Failure modes:

- `x-api-key header is required` instead of `Invalid bearer token` — the
  `anthropic-beta: oauth-2025-04-20` header was not forwarded; check
  `forward_client_headers_to_llm_api` in the rendered configmap.
- 5xx, timeout, or DNS error — the litellm pod cannot reach Anthropic;
  re-run §2 to localize the break.

## Smoke runbook integration

Re-run §1–§3 after:

- Helm chart roll (`gardenio-chart` module apply).
- Terraform changes to the POC VPC, NAT GW, subnets, or security groups.
- ESO upgrades or secret rotations under `gardenio/aws-poc/litellm/*`.
- Anthropic outage / DNS incident — to confirm POC has recovered.

## References

- Epic `ka-0000004609` — AWS POC: enable in-chart litellm + cost tracking.
- Tier h3 task `ka-0000003871` — egress + Anthropic reachability (this doc).
- Chart wiring: `gardenio/helm/gardenio/templates/litellm/{deployment,configmap,service}.yaml`.
- POC overlay: `gardenio/terraform/modules/gardenio-chart/values-aws-poc.yaml.tftpl`.
- Production cutover: [`litellm-production-cutover.md`](./litellm-production-cutover.md).
