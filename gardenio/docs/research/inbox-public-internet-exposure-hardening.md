# Gardenio-in-a-Box — Public-Internet Exposure Hardening (Research)

**Status:** Research / design input.
**Bead:** ka-0000003100 (child of mega-epic ka-0000001295 — "Gardenio in a Box: single-EC2 full install").
**Sibling research:** ka-0000002581 (ingress / edge access control — *owns the edge layer; cross-referenced, not re-derived here*), ka-0000004895 (SSO / identity), ka-0000003889 (EC2 sizing + user-data bootstrap), ka-0000000624 (in-box k8s distro = k3s), ka-0000005069 (image distribution / in-box registry).
**Cross-ref:** ka-0000003889 (EC2 security-group + user-data work).
**Operator:** Matt Baker (oidc:matthew.baker@acme.example).
**Scope:** The *host/node layer* below the edge: host firewall, SSH, keeping the k8s API off the internet, NodePort/hostPort/hostNetwork audit, automatic OS + image patching, fail-closed defaults, and a whole-box threat model for a single-tenant EC2 reachable from the open internet. The edge/TLS/forwardauth/NetworkPolicy/rate-limit layer is owned by ka-0000002581 and is referenced as named controls rather than re-derived.

---

## TL;DR

A single-EC2 "Gardenio in a Box" on the open internet is **safe to build**, because the gardenio Helm chart is already host-clean and ships the right network primitives — but it is **not safe by default**, because the controls that matter are off and today's cloud posture does the opposite of what a public box needs. Seven load-bearing findings (all verified against the repo):

1. **The kube-apiserver is public today.** The POC EKS sets `endpoint_public_access_cidrs = ["0.0.0.0/0"]` (`terraform/composition/poc/main.tf:35-36`). On a single box this **must invert**: k3s :6443 bound to loopback/private (`--bind-address`/`--node-ip`/`--tls-san`), host-firewall-dropped, operator reaches kubectl over an SSH tunnel only. EKS survives `0.0.0.0/0` because a managed IAM proxy fronts it; a single k3s box has no such proxy and co-locates apiserver + kubelet + datastore on one public IP.
2. **The chart opens zero host ports** — no `NodePort`, no `hostPort`, every Service `ClusterIP` (verified by grep across `templates/`). The control-plane exposure risk is entirely k3s-substrate + host-firewall, not gardenio manifests.
3. **The host firewall — not the AWS Security Group — must be the source of truth** ("no cloud deps"). The hard part: k3s programs its own iptables/nftables and a naive `ufw enable` breaks pod networking/DNS. Recommendation: an nftables ruleset that default-drops INPUT to {22,80,443}, leaves FORWARD/NAT to k3s, and trusts loopback + CNI interfaces.
4. **SSH is a brand-new attack surface** (EKS nodes today have no SSH). Key-only, `PermitRootLogin no`, single operator account, modern crypto, nftables :22 rate-limit + fail2ban with the **nftables** banaction (the legacy-vs-nft backend mismatch is the real gotcha). Never bake a shared key into the AMI.
5. **The one node-root-equivalent object is the `gvisor-probe` DaemonSet** (`hostNetwork`+`hostPID`+`privileged`+hostPath `/`). It is **gated OFF by default** (`gvisor.namespace.name=""`) — keep it off in-box unless freeze/thaw is required. **NetworkPolicy cannot protect it** (hostNetwork bypasses pod-IP policy); the controls are RBAC-scoped `pods/exec` + the host firewall.
6. **The entire NetworkPolicy layer is OFF by default** (`networkPolicies.enabled=false`, `defaultDenyEgress=false`). This is the single most important set of defaults to flip: it is what makes every agent-escape hop (→ IMDS, → control plane, → postgres, → other agents) fail closed.
7. **Patching: only OS security packages are safe to fully automate** (k3s recovers cleanly on reboot — pin `--node-ip`/`--tls-san`), with a scheduled reboot window. k3s and image upgrades stay on a controlled cadence (each restarts the apiserver/containerd, re-rendering the runsc config). Gaps: no container-image CVE scanner in CI, tag-only images, and the probe's external `public.ecr.aws/ubuntu:22.04` pull.

**The bottom line:** the box is *fail-closed by omission* today (no app route renders without explicit enablement) — but "in a box" means turning services **on**, so it must become fail-closed **by gate**. See the [public-profile fail-closed checklist](#public-profile-fail-closed-checklist) for the exact defaults that must flip.

---

## Threat Model — Single-Tenant Box on the Open Internet

This section frames the *whole-box* threat model for the "Gardenio in a Box" single-EC2 install (k3s, no managed cloud edge/control-plane). It deliberately does **not** re-derive the edge layer — TLS, forwardauth coverage, rate-limit/`inFlightReq`/`buffering`, and CrowdSec are owned by the ingress/edge doc (ka-0000002581) and are referenced here as named controls. The crown-jewel concern is a **malicious prompt / compromised agent escaping runsc and pivoting to the node, control plane, IMDS, or operator data**.

### Assets and trust boundaries

Five concentric trust boundaries, each a hop an adversary must cross:

```
INTERNET → EDGE (Traefik/forwardauth) → CLUSTER (k3s services) → AGENT SANDBOX (gVisor pod) → NODE (k3s host + control plane + IMDS)
```

| Asset | Where it lives | Why it matters |
|---|---|---|
| Operator data | in-cluster postgres (beads←postgres only), beads daemon | The product's data — beads, project state, decisions |
| LLM credentials & secrets | coopmux credential broker; K8s Secrets; (in-box) Keycloak realm; ExternalSecret-mirrored Claude OAuth / git / registry / NATS tokens (`gvisor-namespace.yaml:182-299`) | Theft = impersonation + spend; lateral access to git/registries |
| Agent execution sandbox | gVisor (`runsc`) agent pods in the PSA-`privileged` gVisor namespace (`gvisor-namespace.yaml:40`) | Runs untrusted model output; the primary attacker-controlled compute |
| The node itself | single k3s host: control plane + kubelet + containerd + `gvisor-probe` (node-root) | Single-tenant box = node compromise is total compromise (no blast-radius wall) |
| Node IAM / IMDS | `169.254.169.254` (if any instance role attached) | Pivot from pod → node credentials → cloud |

Single-tenant collapses the usual multi-tenant wall: there are no "other tenants" to protect, which lowers the authenticated-user adversary — but it *raises* the node-compromise stakes because everything (data, secrets, control plane) shares one host.

### Adversaries and attack trees (STRIDE-lite)

**A1 — Anonymous internet scanner (Spoofing / DoS / Info-disclosure).**
*Path:* mass-scan the public IP → hit Traefik :80/:443, probe for an open k8s API (:6443), NodePort range, or an unauthenticated route.
*Existing control:* No service in the chart is `NodePort` and none uses `hostPort` (verified: `grep -rn "type: NodePort|nodePort:|hostPort" templates/` returns nothing except the gvisor-probe DaemonSet's `hostNetwork`/`hostPID`). Forced HTTP→HTTPS redirect + `sniStrict` TLSOption + oauth2-proxy forwardauth gate browser routes; in a vanilla install **no app-data route renders at all** (ingress doc §"Default posture finding"). Volumetric/brute-force is absorbed by the edge `rateLimit` + (recommended) `inFlightReq`/`buffering`/CrowdSec layer (ingress doc §4-5).
*Residual gap:* The kube-apiserver. EKS today sets `endpoint_public_access_cidrs = ["0.0.0.0/0"]` (`terraform/composition/poc/main.tf:35-36`), wired through `modules/eks/main.tf:105-106` — **the API is on the public internet today.** The box MUST NOT replicate this: k3s :6443 must bind to loopback/private and the host firewall must default-drop everything except 22/80/443. **k3s ServiceLB (Klipper) is a hostPort-like daemonset** — any `type: LoadBalancer` service the operator adds opens a node port directly; combined with a default-drop host firewall this is contained, but it's the one chart-external footgun. (Host firewall + API-binding specifics are owned by sibling doc ka-0000003100; the k3s distro doc pins `--node-ip`/`--tls-san` so a stop/start IP change doesn't break the API cert.)

**A2 — Authenticated-but-malicious / phished operator (Elevation / Repudiation).**
*Path:* steal the operator's Keycloak session (single-tenant ⇒ one human) and drive every forwardauth-gated route as that operator.
*Existing control:* oauth2-proxy hardening — S256 PKCE, `--cookie-secure`, `--cookie-samesite=lax`, per-request CSRF, HTTPS-only issuer (ingress doc §1.5).
*Residual gap:* Lower priority single-tenant (no other tenants to harm), but a phished operator can spawn agents → so this collapses into A3. Mitigation is SSO-track (ka-0000004895) + not exposing `/ci`,`/nats` beyond an operator CIDR `ipWhitelist`.

**A3 — Malicious prompt / compromised agent (the crown jewel — Elevation / Lateral movement).** See dedicated attack tree below.

**A4 — Supply chain (Tampering).**
*Path:* a poisoned base image or dependency lands node-root code via the privileged `gvisor-probe` (`image` default `public.ecr.aws/docker/library/ubuntu:22.04`, `securityContext.privileged: true`, mounts `hostPath: /` at `/host` and `hostPath: /run`; `gvisor-namespace.yaml:159-180`), or a bad agent image runs inside the sandbox.
*Existing control:* gVisor confines the *agent* image; ExternalSecret-templated registry pull-creds; PSA `privileged` is scoped to *only* the gVisor namespace (`gvisor-namespace.yaml:38-39`) leaving the main namespace `baseline`.
*Residual gap:* The probe is the highest-value supply-chain target on the box — it is node-root by construction. Pin the probe image by digest, and (in-box) consider gating `gvisor.probe.create` off entirely if freeze/thaw isn't needed, since it's the single most dangerous object in the chart.

### Crown-jewel attack tree: agent escape → node / control-plane / IMDS

Goal: attacker-controlled agent code reaches node-root, cloud creds, or the control plane. Each hop and the control that blocks it:

| Hop | Attack | Blocking control | Residual gap |
|---|---|---|---|
| Agent code → break out of pod | escape `runsc` to the host kernel | **gVisor (`runsc`)** intercepts syscalls in userspace — the agent never touches the host kernel directly. This is the primary isolation boundary. | A `runsc` 0-day; mitigated in depth by the network hops below |
| Escaped pod → node IMDS | curl `169.254.169.254` to steal node IAM role | **agent-egress NetworkPolicy** excludes `169.254.0.0/16` from its internet rule (`networkpolicies.yaml:362-373`, comment 357-358) | NetworkPolicy is **OFF by default** (`values.yaml:5045`); IMDSv2 hop-limit=1 should belt-and-suspenders this (host/Terraform track) |
| Escaped pod → control plane (:6443) | talk to kube-apiserver, escalate via RBAC | agent-egress excludes all RFC1918 (`10/8`,`172.16/12`,`192.168/16`) + CGNAT `100.64/10` (`networkpolicies.yaml:366-370`); k8s API not public on the box | Same OFF-by-default gap; depends on API not being publicly bound (≠ today's EKS) |
| Escaped pod → postgres / other services | reach operator data directly | **postgres ingress allows beads only** (ingress doc §1.7); agent-egress lists only coopmux:9800, beads-**HTTP**:8080 (gRPC 9090 deliberately excluded, `networkpolicies.yaml:309-319`), NATS:4222, slack-bridge, kube-DNS | OFF-by-default |
| Escaped pod → another agent | pivot tenant-to-tenant | **agent-to-agent isolation**: agent-ingress admits only the controller (+ vmagent); **no agent↔agent** ingress or egress (`networkpolicies.yaml:254-285`) | OFF-by-default; single-tenant lowers impact but agents are still mutually untrusted |
| Escaped pod → `gvisor-probe` (node-root) | reach the privileged probe, ride its `chroot /host` to node-root | hostNetwork on the probe means it is *not* behind a pod IP NetworkPolicy; the real wall is **no inbound listener on the probe** (`command: ["sleep","infinity"]`) + controller-only `pods/exec` RBAC (`gvisor-namespace.yaml:97-100`) | The probe is node-root by design; if reached via a node-local path it is game-over — keep `probe.create` off unless freeze/thaw is required |

**Net:** gVisor blocks hop 1; the agent-egress NetworkPolicy (IMDS + RFC1918 + control-plane + cross-agent exclusions) blocks hops 2-5; the probe's no-listener + RBAC blocks hop 6. The fatal weakness is that **the entire NetworkPolicy layer is disabled by default** (`networkPolicies.enabled=false`, `defaultDenyEgress=false`) — so on a stock install, hops 2-5 are wide open. This is the single most important default to flip. (Full NetworkPolicy derivation: ingress doc §7.)

### Edge rate-limiting (cross-ref, not re-derived)

For A1 (anonymous scanner / volumetric) the control is the edge chain from ingress doc §4-5, slotted in here unchanged: `buffering` (request-body 413 cap) → `inFlightReq` (concurrency 429 ceiling) → `rateLimit` (per-IP token bucket, **RemoteAddr / depth 0** since there's no upstream LB to trust `X-Forwarded-For`) → CrowdSec bouncer (IP-reputation/behavioral, streaming + fail-open). `inFlightReq` and `buffering` do not exist in the chart yet and must be added; `rateLimit` exists per-route but defaults off except beads `/ci`,`/nats`.

### Public-profile fail-closed checklist

Defaults that **MUST flip** for the in-box public profile. "Today" = stock chart/terraform value.

| # | Setting | Today | In-box required | Source |
|---|---|---|---|---|
| F1 | `networkPolicies.enabled` | `false` | **`true`** | `values.yaml:5045` |
| F2 | `networkPolicies.defaultDenyEgress` | `false` | **`true`** | `values.yaml:5059` |
| F3 | `networkPolicies.traefik.{namespaceSelector,podSelector}` | empty | set to k3s `kube-system`/traefik (else all Traefik→svc dropped) | ingress doc §7 |
| F4 | kube-apiserver public exposure | EKS `0.0.0.0/0` | **API bound loopback/private; never public** | `terraform/.../poc/main.tf:35-36` |
| F5 | Host firewall | SG-only (cloud) | **nftables default-drop; only 22/80/443** | sibling ka-0000003100 |
| F6 | SSH | (host) | **key-only, no password** | sibling ka-0000003100 |
| F7 | `keycloak.forwardAuth.enabled` covers **all** routes | on, but per-route gaps | **on + cover every browser route** | ingress doc §2 |
| F8 | `beads.ingress.skipForwardAuth` | `true` | **`false`** | `values.yaml:2354` |
| F9 | All `*.avaIngress.enabled` (AVA carve-outs) | varies | **`false`** (AVA absent ⇒ unauth holes) | ingress doc §2.4 |
| F10 | `trustForwardHeader` / Traefik `forwardedHeaders.trustedIPs` | trust on | **off / unset** (direct exposure ⇒ spoofable) | ingress doc §G2 |
| F11 | `inFlightReq` + `buffering` middleware | absent | **add + default-on** | ingress doc §4 |
| F12 | CrowdSec WAF-lite | absent | **ship default-on, fail-open** | ingress doc §5 |
| F13 | `gvisor.probe.create` | gated off (`name=""`) | **keep off unless freeze/thaw needed**; pin probe image by digest | `gvisor-namespace.yaml:20,125` |
| F14 | k3s ServiceLB `type:LoadBalancer` exposure | Klipper hostPort-like | **no LB services, or rely on F5 default-drop** | distro doc; k3s ServiceLB |

The box is fail-*closed by omission* today (no app route renders without explicit enablement), but "in a box" means turning services **on** — so it must become fail-closed **by gate**: F1/F2 give the network default-deny that makes every agent-escape hop fail closed, and F4/F5 keep the control plane and node off the internet entirely.

---

## Host Firewall (OS-level packet filter on the EC2 box)

### Goal

On a public-internet Gardenio-in-a-Box, the OS packet filter is the source of truth for which TCP/UDP ports are reachable from the internet. The target reachable surface is exactly:

- **443/tcp** + **80/tcp** — Traefik web/websecure (the edge; TLS termination, ACME HTTP-01, forwardauth). The full edge story is owned by the sibling ingress doc (ka-0000002581); this dimension only opens the two ports it needs.
- **22/tcp** — SSH for operator access (ideally further locked to an operator CIDR, not `0.0.0.0/0`).

Everything else must be **unreachable from the internet**: the k3s supervisor + kube-apiserver (**6443/tcp**), kubelet metrics/API (**10250/tcp**), the NodePort range (**30000–32767**), and every in-cluster datastore/bus the chart ships — Postgres **5432**, NATS **4222/8222/8443/6222**, embedded SQLite/kine (file-backed, no port), and the VictoriaMetrics/Grafana metrics stack. The chart already keeps all of these off the host: every Service in `gardenio/helm/gardenio/templates/**/service.yaml` is `ClusterIP` (postgres/service.yaml:10, nats/statefulset.yaml:96, the bridges, beads, beads-viewer, forwardauth, litellm, grafana, vm* — all default or hardcode `ClusterIP`); there is no `type: NodePort` or `type: LoadBalancer` anywhere in the chart, and no `hostPort`. So at the Kubernetes layer the only host-network object is the `gvisor-probe` DaemonSet (`gvisor-namespace.yaml:153-154`, `hostPID: true` + `hostNetwork: true`), which **runs `command: ["sleep","infinity"]` and listens on nothing** — it borrows the host netns for `runsc checkpoint/restore` but opens no socket. The exposure risk is therefore not the chart; it is k3s's own bundled control plane and the host kernel listeners, which a host firewall must contain.

### What exists today vs. the gap

**Today (cloud posture we move away from):** the POC leans entirely on AWS Security Groups and a public EKS endpoint. `terraform/composition/poc/main.tf:35-36` sets `endpoint_public_access = true` and `endpoint_public_access_cidrs = ["0.0.0.0/0"]` — kube-apiserver is wide open to the internet, fronted only by EKS IAM/webhook auth. The VPC module's security groups (`terraform/modules/network/main.tf:217-228`) are the only packet filter. There is **no host-level firewall anywhere in the repo** — no nftables/ufw/firewalld ruleset, and the k3s bootstrap user-data in the sizing doc (`inbox-ec2-sizing-bootstrap.md`) does not configure one.

**The gap for a public box:** "no external cloud dependencies" means we cannot treat the AWS SG as the firewall of record. A box that is correct only because an SG happens to be attached fails the moment it is moved off EC2, the SG is edited, or a second ENI/EIP is added. The host needs its own packet filter that is correct standalone. But on a k3s host that is genuinely hard, because **k3s does not coexist passively with a host firewall** — it programs its own rules and depends on packet forwarding the host firewall will break if configured naively.

### The hard problem: k3s programs its own iptables/nftables

k3s bundles its own networking and writes extensive packet-filter rules at runtime:

- **kube-proxy** installs the Service VIP / NodePort DNAT chains.
- **flannel** (default CNI, VXLAN backend) creates `cni0` and `flannel.1` interfaces and uses **UDP 8472** for pod-to-pod VXLAN. k3s docs explicitly warn 8472 "should not be exposed to the world as it opens up your cluster network to be accessed by anyone."
- **Klipper-LB / ServiceLB** (k3s's bundled `LoadBalancer` provider) is the sharp edge: it runs a **`svclb-*` DaemonSet whose pods use `hostPort` to bind the LoadBalancer's port directly on the node**, then DNATs into the Service. So although *our* chart declares zero `LoadBalancer` Services, **k3s itself creates one** — the bundled Traefik is a `Service type=LoadBalancer`, and ServiceLB binds **80 and 443 on the host** via this hostPort daemonset. This is *why* 80/443 reach Traefik at all on a default k3s install, and it is the one case where a pod legitimately opens a host port. Any host-firewall INPUT policy must allow 80/443 precisely because ServiceLB is listening there.

The classic failure mode: an operator runs `ufw enable` / `ufw default deny`, and **pod networking and DNS break**. The k3s `FORWARD` chain (pod-to-pod and pod-to-Service traffic traverses `FORWARD`, not `INPUT`) gets dropped by ufw's default `DROP` forward policy, kube-dns becomes unreachable, and Services stop resolving — even though the node still answers on 22/80/443. The official k3s guidance is blunt: **"it is recommended to turn off firewalld / ufw."** If you keep a firewall, k3s says you must explicitly allow 6443/tcp and **trust the pod and service CIDRs** (`10.42.0.0/16` pods, `10.43.0.0/16` services) as sources, or networking breaks.

### Concrete recommendation

**1. Do not use ufw or firewalld's high-level "deny forward" defaults; use nftables directly with a filter that only governs INPUT and leaves FORWARD/NAT to k3s.** The safe pattern is: default-drop `input`, default-**accept** `forward` (let kube-proxy/flannel own forwarding), and never flush k3s's own tables. nftables is the right tool on AL2023 (the sizing doc's primary AMI) since AL2023 ships nftables and no ufw.

**2. Build the INPUT allowlist around interfaces, not just ports.** Trust loopback and the CNI interfaces (`cni0`, `flannel.1`) wholesale so intra-cluster traffic (kube-dns, API, datastore Services on their ClusterIPs) is never filtered; restrict the public ENI (`ens5`/`eth0`) to 22/80/443 + established/related + the VXLAN port from the node's own address only (single node: effectively loopback/self).

**3. Treat the host firewall as the source of truth; keep the AWS SG as defense-in-depth only.** If the box still runs on EC2, attaching an SG that also restricts to 22/80/443 is good belt-and-suspenders, but the design must be correct with the SG removed. This is the concrete embodiment of "no cloud deps": the nftables ruleset is committed alongside the user-data bootstrap (extend `inbox-ec2-sizing-bootstrap.md`'s cloud-init), not delegated to Terraform SG rules in `terraform/modules/network`.

**4. Bind k3s's own control plane inward so the firewall is defense-in-depth for 6443, not the only thing standing between the internet and the apiserver.** In `/etc/rancher/k3s/config.yaml` set `https-listen-port` reachable but advertise inward — crucially pin `--node-ip` to the private address and `--tls-san` to the stable name (already recommended by the distro doc, `inbox-k8s-distro-choice.md:71-72,188`), and consider `--kube-apiserver-arg=--bind-address=127.0.0.1` is **not** viable on k3s (the supervisor needs 6443), so the firewall drop on 6443 from the public interface is the real control. kube-apiserver public exposure must be closed here, mirroring the explicit "NOT `0.0.0.0/0`" requirement that the POC violates at `main.tf:36`.

**5. NodePort range:** the chart opens no NodePorts, but k3s can still allocate them. The blanket INPUT drop on the public interface already covers 30000–32767; no special rule needed beyond not opening it.

### Sketch nftables ruleset

```text
#!/usr/sbin/nft -f
# /etc/nftables.conf — Gardenio-in-a-Box host firewall.
# Governs INPUT only. FORWARD/NAT are left to k3s (kube-proxy + flannel).
# DO NOT flush k3s's tables — this adds a separate 'inet filter' table and
# k3s manages its own 'ip'/'ip6' nat/filter tables independently.

table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;

        iif "lo" accept
        ct state established,related accept
        ct state invalid drop

        # Trust the CNI fabric wholesale — pod/Service/DNS traffic must never
        # be filtered, or kube-dns and ClusterIP Services (postgres 5432,
        # NATS 4222, beads, etc.) break.
        iifname { "cni0", "flannel.1" } accept

        # Public edge: only what the internet may reach.
        tcp dport 22 accept                       # SSH (tighten to operator CIDR)
        tcp dport { 80, 443 } accept              # Traefik via ServiceLB hostPort

        icmp type echo-request limit rate 5/second accept
        icmpv6 type { nd-neighbor-solicit, nd-router-advert, nd-neighbor-advert } accept

        # Flannel VXLAN: single node only needs it from itself; never the world.
        # (On multi-node, replace with: ip saddr <node-cidr> udp dport 8472 accept)
        # Default-drop below already blocks 8472 from the public interface.

        # Everything else (6443 apiserver, 10250 kubelet, 30000-32767 NodePort,
        # 5432, 4222/8222/8443/6222, metrics) falls through to policy drop.
    }

    # Leave forward to k3s; do NOT set policy drop here.
    chain forward { type filter hook forward priority 0; policy accept; }
    chain output  { type filter hook output  priority 0; policy accept; }
}
```

Apply via cloud-init *after* k3s is up (so k3s's tables already exist and we only add ours), then `systemctl enable --now nftables`. Validate the two halves separately: from off-box, `nc -vz <eip> 6443` / `10250` / `5432` must all fail while `443` succeeds; on-box, `kubectl get pods -A` and pod DNS must still work (proves FORWARD/CNI were not broken).

---

## SSH Hardening for the Public Box

### (a) What exists today

There is **no SSH posture to harden today** — the current POC has no host you log into. The POC runs on EKS (`gardenio/terraform/composition/poc/main.tf`), whose worker nodes are a managed `aws_eks_node_group` (`gardenio/terraform/modules/eks/main.tf:264`) with **no `remote_access` / `key_name` / `ec2_ssh_key` block** — i.e. operators never SSH to nodes; they reach the control plane over the public kube-apiserver endpoint (`endpoint_public_access = true`, `endpoint_public_access_cidrs = ["0.0.0.0/0"]`, `main.tf` POC block). The only Terraform that touches EC2-adjacent infra is `terraform/bootstrap/` (S3 state + DynamoDB lock — no instances, no key pairs). So for "Gardenio in a Box" on a single public EC2 running k3s, **SSH is a brand-new attack surface that must be designed from scratch**, not a migration of an existing config.

This dimension is the host-level half that the edge research explicitly defers to it. `gardenio-in-a-box-ingress-edge-access-control.md` (ka-0000002581) states the host layer must expose "only 80/443/22 reachable; control plane unreachable from outside" via "SG + nftables exposing only 22/80/443" and keeps "**fail2ban as a host-level SSH/log jail supplement, not the primary HTTP defense**" — the HTTP edge there is CrowdSec/Traefik. SSH on :22 is therefore the one *interactive* internet-facing port the edge doc does not cover, and it is the focus here. The k3s install sketch (`inbox-k8s-distro-choice.md` §"Single-EC2 install sketch") provisions everything through **user-data / cloud-init** with `/etc/rancher/k3s/config.yaml` pre-staged — that same user-data is where all of the below must land.

### (b) The gap for a public box

1. **No `sshd_config` baseline.** Stock AL2023/Ubuntu cloud images already ship `PasswordAuthentication no` for the default user, but they leave `PermitRootLogin prohibit-password`, default `MaxAuthTries 6`, `LoginGraceTime 2m`, and broad `AllowTcpForwarding/X11Forwarding` enabled — none of it scoped to a single operator account. On the open internet :22 will see continuous credential-stuffing within minutes.
2. **No brute-force throttle.** Nothing bans repeat offenders, and there is no host firewall rate-limit on :22.
3. **No defined key-provisioning story.** There is no decision on how the operator's pubkey reaches a fresh box, and the air-gap/baked-AMI path (`inbox-k8s-distro-choice.md`) tempts a baked shared key — which would be a single stolen key for every customer box.
4. **Firewall-backend mismatch risk.** k3s installs its own iptables/nf_tables rules; a brute-force banner (fail2ban) that writes to a *different* backend than the host's active firewall silently no-ops. This is the central host-firewall gotcha for the box and must be pinned.

### (c) Concrete recommendation

**1. `sshd_config` drop-in.** Do not edit the vendor `/etc/ssh/sshd_config`; write `/etc/ssh/sshd_config.d/10-gardenio-hardening.conf` from user-data (both AL2023 and Ubuntu honor the `.d` Include). Contents:

```
# Auth
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
AuthenticationMethods publickey
PermitEmptyPasswords no
UsePAM yes

# Scope to the single operator account
AllowUsers gardenio-op

# Brute-force pressure
MaxAuthTries 3
MaxSessions 4
LoginGraceTime 20

# Lateral-movement / tunneling surface
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding no        # the box is the destination, not a jump host
PermitTunnel no
GatewayPorts no

# Idle hygiene
ClientAliveInterval 300
ClientAliveCountMax 2

# NOTE: do NOT pin Ciphers/MACs/KexAlgorithms. On AL2023 these are governed by
# the system-wide crypto-policy (update-crypto-policies); a drop-in listing
# algorithms the running OpenSSH build doesn't advertise (e.g.
# sntrup761x25519-sha512@openssh.com on the AL2023 OpenSSH) makes sshd reject
# its own config and fail to listen — locking the operator out (ka-0000006491,
# observed on a live box; required EBS volume surgery to recover). The security
# directives above are what matter; leave algorithm selection to the distro.
```

Notes: `AllowUsers gardenio-op` (a dedicated non-root sudoer created earlier in user-data) is preferred over `AllowGroups` for a single-operator box — one fewer indirection. `AllowTcpForwarding no` is safe because the box is a terminal destination; operators reach in-cluster services via Traefik on 443, not SSH tunnels. If a kubectl-over-SSH tunnel is ever needed, relax to `AllowTcpForwarding local` rather than `yes`. Apply with `sshd -t && systemctl reload sshd` at the end of the user-data run so a typo fails the boot loudly instead of locking you out silently.

**2. Key provisioning — never bake a shared key.** Use the **EC2 key pair → cloud-init** path: the operator supplies their own public key as the launch key pair (or via `cloud-init` `ssh_authorized_keys:` / user-data), and cloud-init drops it into `~gardenio-op/.ssh/authorized_keys`. This keeps the private key on the operator's laptop and produces a **per-box** trust anchor. Critically, if the box image is a **baked/air-gap AMI** (the `inbox-k8s-distro-choice.md` install option), the AMI must contain **zero `authorized_keys`** — the key must be injected at launch, never baked, so a leaked image cannot unlock every customer's box. Disable the cloud images' default behavior of also writing the key to a vendor account (`ec2-user`/`ubuntu`) if you want :22 to admit only `gardenio-op`.

**3. Brute-force banning — pin the firewall backend (the real gotcha).** k3s on a modern host uses **nftables**, but fail2ban's default `banaction` on many distros is `iptables-multiport`, which writes to **iptables-legacy** — on a pure-nftables host those rules are in a different table that never matches inbound :22, so fail2ban *looks* healthy while banning nothing. Pin it explicitly in `/etc/fail2ban/jail.local`:

```ini
[DEFAULT]
banaction = nftables-multiport
banaction_allports = nftables-allports
backend = systemd            # journald is the log source on AL2023/Ubuntu k3s hosts
bantime  = 1h
findtime = 10m
maxretry = 4

[sshd]
enabled = true
port    = ssh

[recidive]
enabled  = true
logpath  = /var/log/fail2ban.log
bantime  = 1w
findtime = 1d
maxretry = 5
```

`backend = systemd` matters because k3s hosts typically have no `/var/log/auth.log`; sshd logs to journald. The `recidive` jail escalates repeat-offender IPs (those that earn a `[sshd]` ban ≥5×/day) to a week-long ban. **Alternatives:** `sshguard` (lighter, nftables-native, fewer moving parts) is a fine swap if fail2ban's Python footprint is unwanted on a small instance; or, since `MaxAuthTries 3` + key-only auth already makes bans largely cosmetic, simply **rate-limit :22 in the host nftables ruleset** (e.g. an `ssh-meter` set limiting new conns to ~10/min/src), which has no daemon at all. Recommended order for the box: nftables rate-limit on :22 as the always-on floor, **plus** fail2ban with the nftables banaction for IP reputation/recidive — both write to the same nftables backend, so they compose cleanly. Whatever is chosen, it shares the host firewall with k3s and the edge doc's "only 22/80/443" rule, so all of it **must be nftables** to avoid the legacy-vs-nft split.

**4. Should SSH even be internet-exposed?** The "no external cloud dependencies" constraint is decisive. **SSM Session Manager** would remove :22 from the internet entirely but is an AWS dependency (SSM agent + IAM + AWS endpoints) — disqualified by the box charter. **Tailscale/WireGuard** would also hide :22 behind a private overlay, but Tailscale adds a SaaS coordination-server dependency (disqualified), and self-hosted WireGuard/headscale is a real option that trades one open port (UDP :51820, silent — drops unauthenticated packets, so it does not even answer scanners) for the SSH port. **Recommendation:** ship **plain hardened public SSH** (key-only + nftables rate-limit + fail2ban) as the default, because it has zero external deps and matches the edge doc's "22/80/443 open" model. Offer **self-hosted WireGuard with SSH bound to the wg interface only** (`ListenAddress` on the wg IP, :22 firewalled off the public NIC) as an opt-in hardening profile for operators who want SSH off the public internet without taking a cloud dependency. With key-only auth and a 3-try limit, the residual brute-force risk on public :22 is low; WireGuard is defense-in-depth, not a prerequisite.

**5. Where it lands in bootstrap.** All of the above is **user-data / cloud-init**, the same mechanism `inbox-ec2-sizing-bootstrap.md` (ka-0000003889) and `inbox-k8s-distro-choice.md` use to stage `/etc/rancher/k3s/config.yaml` and install k3s. Concrete ordering in the bootstrap script:
- create `gardenio-op` user + sudoers; `cloud-init` injects the launch-time pubkey into its `authorized_keys`;
- write `/etc/ssh/sshd_config.d/10-gardenio-hardening.conf`, then `sshd -t && systemctl reload sshd`;
- install + enable fail2ban (`jail.local` above) **after** confirming the host firewall is nftables (`update-alternatives --set iptables /usr/sbin/iptables-nft` on Debian/Ubuntu, or verify `nft list ruleset` is the live path on AL2023);
- add the nftables :22 rate-limit rule to the same ruleset that exposes 80/443 (per the edge doc's host firewall);
- run the k3s install **after** sshd is reloaded, so a config typo aborts before k3s mutates the firewall and you still have a working session.

---

## Kubernetes API Server NOT Exposed Publicly + Control-Plane Port Hardening

This dimension answers one question for the public box: **a process on the open internet must never be able to reach the kube-apiserver, the kubelet, or any other control-plane port.** The edge layer (Traefik/TLS/forwardauth/NetworkPolicy, ka-0000002581) protects the *application* surface on 80/443; this dimension protects the *control plane* underneath it. They are the two halves of "only 80/443/22 reachable; control plane unreachable from outside" — a framing the edge doc explicitly defers to this work (`gardenio-in-a-box-ingress-edge-access-control.md:227`).

### (a) What exists today — the cloud posture we move AWAY from

The POC's kube-apiserver is **public to the entire internet**. `gardenio/terraform/composition/poc/main.tf:35-36` sets:

```hcl
endpoint_public_access       = true
endpoint_public_access_cidrs = ["0.0.0.0/0"]
```

These flow straight into the EKS cluster's `vpc_config` (`gardenio/terraform/modules/eks/main.tf:104-106`: `endpoint_public_access = var.endpoint_public_access`, `public_access_cidrs = ... endpoint_public_access_cidrs`). The in-repo comment is candid that this is "public-open … reachable from the public internet with IAM / OIDC as the access gate," explicitly authorized only because the cluster "lives in an isolated AWS account … with no production data" (`main.tf:27-34`). On EKS this is *survivable* because the managed control plane fronts the apiserver with AWS IAM authentication and an AWS-managed TLS endpoint, and there is no kubelet/etcd port reachable from outside the VPC.

The two patterns that make this safe on EKS **do not exist on a single box**: there is no managed IAM auth proxy in front of k3s's apiserver, and on one host the apiserver, the kubelet (10250), kube-proxy, and the kine/SQLite datastore all live on the *same* public IP. So the EKS `0.0.0.0/0` default cannot be ported — it must be inverted to "bound away from the public NIC + host-firewall-dropped."

### (b) The gap for a public box

k3s's apiserver listens on **TCP 6443**, the supervisor/agent-join API also on **6443** (k3s collapses the separate 9345 supervisor port into 6443 in the default single-server bundle), the **kubelet read/write API on 10250**, and **kube-proxy health on 10256**. By k3s default the apiserver `--bind-address` is `0.0.0.0`, i.e. it binds every interface including the public ENI. On EC2, the AWS Security Group is the *only* thing between `0.0.0.0:6443` and the internet — and "Gardenio in a Box" explicitly rejects reliance on an AWS SG as the sole firewall (no cloud-dependency). A single mis-set SG rule, or a future move off AWS, leaves the kubelet (10250) and apiserver (6443) directly dialable. 10250 in particular is catastrophic if exposed: it serves `/exec`, `/run`, and `/logs` and is a direct container-RCE vector.

**The chart is clean — the gap is purely host/k3s-level, not chart-level.** Verified across `gardenio/helm/gardenio/templates/`:
- **Zero `NodePort` services** and **zero `nodePort:` fields** (grep returns nothing).
- **Zero `hostPort`** declarations.
- Every Service is `ClusterIP`, `ExternalName`, or operator-overridable defaulting to `ClusterIP` (e.g. `templates/postgres/service.yaml:10`, `templates/nats/statefulset.yaml:96`, `templates/forwardauth/service.yaml:12`, `templates/litellm/service.yaml:10` are hard `ClusterIP`; beads/coopmux/controller/etc. default `ClusterIP`).
- The **only** `hostNetwork`+`hostPID`+`privileged` object is the `gvisor-probe` DaemonSet (`templates/gvisor-namespace.yaml:153-164`), and it exposes **no listening port** — its container is `command: ["sleep", "infinity"]` (`:162`), driven only by the controller via `pods/exec`. So even on `hostNetwork` it adds no internet-reachable port.

Net: nothing in the gardenio chart opens a host port. **The entire control-plane exposure risk on the box is owned by k3s defaults and the host firewall, not by gardenio's manifests.**

### (c) Concrete recommendation

**1. Host firewall is the primary control (the cloud-independent one).** Drop inbound 6443, 10250, 10256, and the NodePort range at the host, so exposure does not depend on the AWS SG:

```
# nftables / iptables INPUT, applied in user-data before/with k3s install
# Allow only loopback + (optionally) the private VPC CIDR to control-plane ports
iptables -A INPUT -i lo -j ACCEPT
iptables -A INPUT -p tcp --dport 22  -j ACCEPT        # SSH (operator entry)
iptables -A INPUT -p tcp --dport 80  -j ACCEPT        # Traefik
iptables -A INPUT -p tcp --dport 443 -j ACCEPT        # Traefik TLS
iptables -A INPUT -p tcp --dport 6443  -j DROP        # apiserver/supervisor
iptables -A INPUT -p tcp --dport 10250 -j DROP        # kubelet (RCE if open)
iptables -A INPUT -p tcp --dport 10256 -j DROP        # kube-proxy health
iptables -A INPUT -p tcp --dport 9345  -j DROP        # supervisor (if split)
iptables -A INPUT -p tcp --dport 30000:32767 -j DROP  # NodePort range (defense-in-depth; chart uses none)
```

This is the **same "only 22/80/443 reachable"** host posture the edge doc names as ka-0000003100's responsibility (`gardenio-in-a-box-ingress-edge-access-control.md:227`). The SG should be configured identically as a second layer, but the box must not *depend* on it.

**2. Bind the apiserver away from the public NIC via k3s flags in `/etc/rancher/k3s/config.yaml`.** This reuses the `--node-ip` / `--tls-san` pinning already recommended in the distro-choice doc — pin them so an EC2 stop/start that re-IPs the node does not invalidate cert SANs (`inbox-k8s-distro-choice.md:71-72, 188-189`, "pin `--node-ip` / `--tls-san`"; open question #9 at `:188-189`):

```yaml
# /etc/rancher/k3s/config.yaml
node-ip: 10.0.1.23                 # pinned private IP (survives stop/start)
advertise-address: 10.0.1.23       # what the apiserver advertises to nodes
bind-address: 127.0.0.1            # apiserver listens on loopback only
tls-san:
  - 10.0.1.23                      # private IP the operator tunnels to
  - box.internal.example.com       # DNS name the operator's kubeconfig uses
# (do NOT add the public IP/DNS to tls-san unless the operator genuinely
#  reaches it that way; an unused public SAN is needless cert surface)
```

Note `--bind-address 127.0.0.1` is the strongest stance but means *all* kubectl access (even from the node) goes through loopback — which is exactly what we want for a single box. If the kubelet/agent components need to reach the apiserver on the node, k3s's loopback `127.0.0.1:6443` already serves them. For the rare case where a specific apiserver flag isn't surfaced as a k3s top-level option, use `--kube-apiserver-arg` (the same hook the distro doc already uses for the PodSecurity `AdmissionConfiguration`, `inbox-k8s-distro-choice.md:126-128, 146-148`), e.g. `kube-apiserver-arg: ["bind-address=127.0.0.1"]`.

**3. Operator reaches kubectl over an SSH tunnel, never a public endpoint.** k3s writes the admin kubeconfig to `/etc/rancher/k3s/k3s.yaml` as root, mode `0600`, with `server: https://127.0.0.1:6443`. The operator does **not** copy this file to a laptop and repoint it at a public IP. Instead:

```
ssh -L 6443:127.0.0.1:6443 ec2-user@box.internal.example.com   # tunnel loopback:6443
# locally: kubeconfig server: https://127.0.0.1:6443
kubectl get pods
```

Because the cert's SANs must cover the address the *client* validates, `127.0.0.1` is already a default k3s SAN; if the operator instead tunnels to and validates against `box.internal.example.com`, that name must be in `tls-san` (point 2). This keeps 6443 firewall-dropped (point 1) while still giving the operator full kubectl — the tunnel terminates on the host's loopback, which the DROP rule explicitly exempts via `-i lo -j ACCEPT`.

**4. Anonymous-auth / RBAC posture.** k3s ships RBAC enabled and the apiserver with `--anonymous-auth=false` by default, and the **kubelet** defaults to `authorization-mode=Webhook` + `anonymous-auth=false` (unlike upstream kubeadm where kubelet anon-auth historically defaulted on). So no extra hardening flag is strictly required to close anonymous access. As belt-and-suspenders for a public box, explicitly assert it so a future k3s default change can't regress the kubelet:

```yaml
kubelet-arg:
  - "anonymous-auth=false"
  - "authorization-mode=Webhook"
```

This is cheap and makes the posture self-documenting; it is **not** a substitute for the firewall — 10250 must be dropped at the host regardless (point 1), because even authenticated kubelet exposure is unnecessary attack surface on a single box.

**Contrast with the BYO profile (not public-safe).** The BYO profile (`docs/byo-cluster.md:48-49`) runs auth **off** (`keycloak.enabled=false`, `forwardAuth.enabled=false`) and reaches services via `kubectl port-forward` against `ClusterIP` with `traefik.enabled=false`. That is fine for a private cluster the operator already controls, but it is explicitly **not** the public-box posture: it assumes the apiserver itself is already private (the user's own cluster) and provides no host-firewall story. The public box keeps the edge auth stack **on** and additionally locks the control plane at the host — BYO is a different threat model and must not be conflated with the public profile.

### Summary table

| Port | Service | Today (EKS POC) | Public box requirement |
|---|---|---|---|
| 6443 | kube-apiserver + supervisor | public via SG `0.0.0.0/0` (`main.tf:36`) | `bind-address=127.0.0.1`; host DROP; operator via SSH tunnel |
| 10250 | kubelet read/write (RCE vector) | not internet-reachable (in-VPC) | host DROP; `anonymous-auth=false` (k3s default, assert) |
| 10256 | kube-proxy health | n/a | host DROP |
| 9345 | supervisor (if split from 6443) | n/a | host DROP |
| 30000–32767 | NodePort range | n/a | host DROP (defense-in-depth; chart declares none) |
| 80/443 | Traefik ingress (app surface) | — | allowed; protected by edge layer (ka-0000002581) |
| 22 | SSH (operator entry) | — | allowed; sole legitimate path to the apiserver |

---

## NodePort / hostPort / hostNetwork Audit on a Single Public Node

This dimension answers a narrow but load-bearing question for Gardenio-in-a-Box: **which ports and host namespaces does the chart (plus the k3s substrate it now runs on) actually expose on a single node that sits on the open internet?** The edge/TLS/forwardauth/NetworkPolicy layer is covered in the sibling ingress doc (ka-0000002581) and is not re-derived here. The headline finding is good news: the chart itself opens nothing on the host. The risk surface is two-fold — one node-root-equivalent pod the chart can render (the gVisor probe), and the k3s-bundled ServiceLB, which is what *actually* turns the chart's one in-box `LoadBalancer` Service into host ports.

### (a) What exists today — the chart is host-clean

A full grep of `gardenio/helm/gardenio/` confirms the chart opens **zero** host ports:

- **Zero NodePort Services.** No template or value sets `type: NodePort`. Every Service is `ClusterIP` (the default for all `*.service.type` values, e.g. `beads/service.yaml:10`, `controller/service.yaml:14`, `grafana/service.yaml:10`) or `ExternalName` (the two fleet aliases `fleet/beads-external-service.yaml:15`, `fleet/nats-external-service.yaml:15`, which resolve a DNS name and bind no port). `values.yaml` sets `type: ClusterIP` at every service block (lines 424, 1234, 2041, 2432, 2853, 3105, 3176, 3364, 3521, 3620, 3756, 3973, 4048, 4886). NATS is `ClusterIP` (`nats/statefulset.yaml:96`).
- **Zero `hostPort` container ports.** `grep -rn hostPort gardenio/helm/gardenio/` returns nothing in any template — only prose mentions in the gVisor namespace comments.
- **Exactly one `hostNetwork: true` object** in the entire chart: the gVisor probe DaemonSet (`templates/gvisor-namespace.yaml:154`). The same object is the only `hostPID: true` (`:153`) and the only `privileged: true` (`:164`).

This is the posture the box wants and a sharp contrast with the cloud POC, whose EKS control plane is published to the world — `terraform/composition/poc/main.tf:35-36` sets `endpoint_public_access = true` and `endpoint_public_access_cidrs = ["0.0.0.0/0"]`. The box must keep the kube-apiserver off the public interface entirely (host-firewall / `--bind-address` / `--tls-san` dimension; not this chart's job).

### (b) The gap — the one node-root-equivalent pod: gvisor-probe

The gVisor probe is, by construction, a **node-root pod**. `templates/gvisor-namespace.yaml:153-180`:

- `hostPID: true` + `hostNetwork: true`
- container `securityContext.privileged: true`
- mounts the host root `hostPath: /` at `/host` and `hostPath: /run` at `/run`
- lives in a namespace labelled `pod-security.kubernetes.io/enforce: privileged` (`:40`)

The controller drives it via `pods/exec` (RBAC granted at `:97-100`) and runs `chroot /host bash -c <runsc checkpoint|restore>` (design comment `:128-132`). With `chroot /host` over a hostPath-`/` mount plus the host PID namespace and privileged caps, this pod **is** the node: it can read every secret on disk, every other container's filesystem, the kubelet's credentials, and on a single box that means the entire control plane and every other agent.

**Blast radius on a single public box if an agent escapes runsc and reaches the probe:** an agent already runs under gVisor (runsc) precisely because it is untrusted. If a sandbox escape lands it in the gVisor namespace, the probe is the highest-value pivot in the cluster — exec/chroot into it yields node root, and on a one-node box node root is cluster root. There is no second node to contain the blast.

**Critical caveat — NetworkPolicy cannot help here.** Because the probe is `hostNetwork: true`, its traffic uses the node's network namespace, not a pod IP, so **pod-selector NetworkPolicies do not apply to it** — the default-deny mesh described in the ingress doc silently does not cover this pod. This must be called out explicitly: you cannot firewall the probe with a Kubernetes NetworkPolicy. The controls that *do* apply are admission/RBAC and the host firewall.

**Recommendations (concrete):**
1. **Keep it gated and OFF by default.** The whole file renders nothing unless `gvisor.enabled` AND `gvisor.namespace.name` are set, and the probe additionally needs `gvisor.probe.create` (`:125`). Shipped defaults are safe: `gvisor.namespace.name: ""` (`values.yaml:825`) renders the entire file empty. The in-box profile should leave gVisor/freeze-thaw **disabled** unless checkpoint/restore is a hard requirement — on a single box the snapshot-per-node warm-pool rationale (`values.yaml:806-807`) largely evaporates anyway.
2. **If enabled, lock down who can exec.** The `pods/exec` `create` verb in the gVisor namespace is the only thing standing between a foothold and node root. The chart grants it solely to the controller SA (`gvisor-namespace.yaml:121-124`). Audit that no broader RBAC (e.g. the `clusterAdmin` / `extra-subjects` / `pods/exec` grants referenced around `values.yaml:1471` and the cluster-admin tests) lets an agent SA reach `pods/exec` in this namespace. No human/agent kubeconfig on the box should carry `pods/exec` on `gardenio-gvisor`.
3. **Do not rely on NetworkPolicy for this pod** (see caveat); rely on keeping it absent, RBAC-scoping the exec, and the host firewall.

### runsc thaw-state hostPath on each agent pod

Separately from the probe, **every runsc agent pod carries a per-agent thaw-state `hostPath`** (chart comment `gvisor-namespace.yaml:6`, sourced to `podmanager/volumes.go:177`). This is why the agents need the PSA-`privileged` namespace at all — `baseline` PSA forbids `hostPath` (`values.yaml:813`). Analysis: a `hostPath` volume is a node-local directory bind-mounted into the agent. It is far weaker than the probe (no `privileged`, no `hostPID`, no `/`-root mount — it's a scoped path for the checkpoint image), but it is still a **write channel onto the node filesystem from inside an untrusted sandbox**, and it forces the agent namespace down from `baseline` to `privileged`, which removes the PSA backstop that would otherwise reject a privileged or host-namespace pod. On a public box this compounds with the probe: the same namespace that holds the agents' hostPaths also admits the node-root probe. Recommendation: same as above — keep freeze/thaw off in-box unless required; if required, treat the entire `gardenio-gvisor` namespace as a node-trust boundary, not an app namespace.

### Klipper-LB / ServiceLB — the real host-port channel

The chart opens no host ports, but **k3s does**, via bundled ServiceLB (Klipper-LB). For every `type=LoadBalancer` Service, ServiceLB creates a `svclb-*` DaemonSet in `kube-system` whose pods **bind the service ports as `hostPort` on each node** (k3s networking docs). On a single box this means: a `type=LoadBalancer` Service = a host port reachable subject only to the host firewall — Kubernetes adds no further gate.

In-box, exactly **one** Service becomes `type=LoadBalancer`: the **k3s-bundled Traefik**, whose default LoadBalancer publishes **:80 and :443** (k3s docs; ServiceLB binds those as hostPorts via `svclb-traefik`). The Gardenio chart deliberately **does not ship Traefik** and assumes a cluster-level install — for the box that is the k3s-bundled Traefik (ingress doc §1.1, `gardenio-in-a-box-ingress-edge-access-control.md:32`). **Every Gardenio Service stays `ClusterIP`** (verified above), so nothing else in the chart triggers a `svclb-*` DaemonSet. The k3s distro choice keeps ServiceLB precisely so this one LB Service works without MetalLB (`inbox-k8s-distro-choice.md:62`, `:152`).

**Implication:** the host firewall — not Kubernetes — is the control for these ports. The :80/:443 hostPorts must be exactly the ports the box intends to expose, and ServiceLB must not be allowed to publish anything else. Because ServiceLB only binds ports of `LoadBalancer` Services, and Traefik is the only one, the surface is bounded *as long as nothing else flips to `LoadBalancer`*. Two guard rails: (1) keep all chart `*.service.type` at the `ClusterIP` default — never set any to `LoadBalancer`; (2) at the distro layer, if Traefik's two ports are the entire intended LB surface, this is fine as-is; if any tighter control is wanted, `--disable=servicelb` is available but then Traefik needs an alternative path to :80/:443 — out of scope for this dimension, owned by the host/edge layers.

### General principle and port-exposure table

**Every port that reaches the host must be one of {22, 80, 443}; everything else is cluster-internal (ClusterIP) and unreachable from the node's network interface.** SSH (22) is host-level (sshd, not k8s). 80/443 arrive only via the single Traefik LoadBalancer → `svclb-traefik` hostPorts. Nothing in the Gardenio chart adds a host port.

| Port | Reaches host? | Mechanism | Owner | In-box disposition |
|---|---|---|---|---|
| 22 | Yes | host sshd | host OS | Host firewall only (not k8s). Keep tight (key-only). |
| 80 | Yes | `svclb-traefik` hostPort (ServiceLB) → bundled Traefik `:web` | k3s | Intended. HTTP→HTTPS redirect (ingress doc §1.2) or ACME HTTP-01. |
| 443 | Yes | `svclb-traefik` hostPort (ServiceLB) → bundled Traefik `:websecure` | k3s | Intended. All app IngressRoutes terminate here. |
| 6443 (kube-apiserver) | **Must NOT** | k3s server | k3s | Bind to loopback/private + `--tls-san`; host firewall blocks public. (Sibling dimension — *not* `0.0.0.0/0` like the EKS POC, `poc/main.tf:35-36`.) |
| beads HTTP/gRPC, postgres 5432, NATS 4222, coopmux, litellm, grafana, vmsingle, the bridges, forwardauth :4180, roles-server, edge-auth | No | ClusterIP only | Gardenio chart | Stay ClusterIP (verified). In-cluster only; exposed publicly *only* through Traefik IngressRoutes, which is the edge layer's concern. |
| gvisor-probe (hostNetwork/hostPID) | Host namespace, no listening port | DaemonSet, `sleep infinity` + exec | Gardenio chart (gated OFF) | Keep off in-box; if on, RBAC-scope `pods/exec`; NetworkPolicy does **not** apply (hostNetwork). |
| Any NodePort range (30000–32767) | No | — | — | Chart renders no NodePort; range must never be opened at the host firewall. |

Net: on the chart side the audit is clean (zero NodePort, zero hostPort, one gated host-namespace pod). The two things that genuinely reach the host are (1) the k3s ServiceLB hostPorts for Traefik's 80/443 — governed by the host firewall, and (2) the gVisor probe's host-namespace foothold — governed by keeping it disabled and RBAC-scoping exec, *not* by NetworkPolicy.

---

## Automatic OS + Image Patching for an Unattended Public Box

A single, internet-facing EC2 box with no on-call has only one viable patch posture: **unattended security patching with a scheduled reboot window**, plus a **controlled (not fully-automatic) cadence for the disruptive layers** (k3s, container images). Below, each layer is separated into (a) what exists today, (b) the gap for a public box, (c) the concrete recommendation.

### Scope note: the edge layer is owned elsewhere

This dimension covers *currency of the bits running on the box* (kernel/OS packages, k3s, container images, base images). The network-exposure surface that those patches protect — Traefik/TLS/forwardauth/NetworkPolicy and the fact that the kube-apiserver must **not** be world-reachable (today's POC sets `endpoint_public_access_cidrs = ["0.0.0.0/0"]`, `terraform/composition/poc/main.tf:36` — the box must do the opposite) — is covered in `gardenio-in-a-box-ingress-edge-access-control.md` (ka-0000002581). Patching reduces the *blast radius of a CVE*; it does not replace the edge controls.

### 1. OS patching (the EC2 host)

**(a) Today.** The in-box host is provisioned by user-data/cloud-init onto an `AL2023 (or Ubuntu)` AMI; the distro doc explicitly leaves AMI open as `AL2023 (or Ubuntu)` (`inbox-k8s-distro-choice.md:132`). There is no OS-patching automation in the repo — the cloud POC relied on EKS-managed nodes, which the box deliberately moves away from.

**(b) Gap.** An unattended public box accrues kernel + userland CVEs (OpenSSL, glibc, sshd, sudo) with nobody to `dnf upgrade`. Security-relevant fixes must apply themselves; kernel/glibc fixes additionally need a reboot to take effect.

**(c) Recommendation — security-only auto-apply + one scheduled reboot window:**

- **AL2023:** `dnf-automatic` configured for `upgrade_type = security`, `apply_updates = yes`, with `reboot = when-needed` (or a separate timed reboot) confined to a maintenance window. Concretely, in user-data drop `/etc/dnf/automatic.conf` with `[commands] upgrade_type=security`, `apply_updates=yes`, then `systemctl enable --now dnf-automatic.timer`. Schedule the reboot via a systemd timer (e.g. a `dnf-needs-reboot` check + `systemctl reboot` at 04:00 local) rather than `dnf-automatic`'s own reboot, so the window is explicit.
- **Ubuntu:** `unattended-upgrades` (origins limited to `${distro_id}:${distro_codename}-security`), `needrestart` in batch mode (`/etc/needrestart/needrestart.conf` → `$nrconf{restart} = 'a'`) so daemons relink against patched libs without prompting, and `Unattended-Upgrade::Automatic-Reboot "true"` with `Automatic-Reboot-Time "04:00"`.

**Reboot tradeoff (single non-HA box = downtime).** Auto-reboot on a one-node appliance is a hard outage, not a rolling drain — so pin it to a low-traffic window rather than `when-needed`-immediately. This is acceptable specifically because k3s recovers cleanly on reboot: `k3s.service` runs `Restart=always` over an on-disk SQLite/kine datastore, giving "clean unattended reboot recovery, no etcd quorum, no IP/cert breakage" provided `--node-ip` / `--tls-san` are pinned so an instance stop/start that changes the private IP does not invalidate the apiserver cert SANs (`inbox-k8s-distro-choice.md:70-71, 189`). A reboot is therefore safe to automate; an *uncontrolled-time* reboot is the only thing to avoid.

### 2. k3s itself

**(a) Today.** The distro choice is k3s pinned to the v1.33 line via `INSTALL_K3S_VERSION` (e.g. `v1.33.12+k3s1`), installed by the `get.k3s.io` one-liner or the air-gap `INSTALL_K3S_SKIP_DOWNLOAD=true ./install.sh` flow (`inbox-k8s-distro-choice.md:8-9, 137-139`). There is no upgrade automation; `system-upgrade-controller` is referenced nowhere in the repo except as a possibility.

**(b) Gap.** A k3s version goes EOL and accumulates apiserver/kubelet CVEs. But a k3s upgrade on a single node restarts the apiserver — a brief control-plane outage and a containerd restart, which is the same moment the **runsc containerd config template** (`config-v3.toml.tmpl`) is re-rendered (`inbox-k8s-distro-choice.md:105`). An uncontrolled auto-upgrade that clobbers the runsc handler would break the freeze/thaw substrate — the non-negotiable priority.

**(c) Recommendation — pin + controlled cadence, NOT fully-auto.** Keep `INSTALL_K3S_VERSION` pinned and upgrade on a deliberate monthly/quarterly cadence (re-run the installer with a bumped pin, or stage Rancher's `system-upgrade-controller` with a `Plan` CR but **leave it suspended/manually-triggered**). Each upgrade must re-verify that the `config-v3.toml.tmpl` runsc stanza and the DaemonSet/restore `--root` path still match after containerd restarts (`inbox-k8s-distro-choice.md:144-145, open-question 2`). Single-node = no HA = treat k3s upgrades as a planned maintenance event in the same reboot window, never as an unattended self-update.

### 3. Container image patching

**(a) Today.** Images are built in `.rwx/docker.yml` and pushed to `<registry>/gardenio/<component>:<calver>`. For the box, the air-gap path is an OCI-layout bundle seeded into an in-box `zot` registry on `127.0.0.1:5000` and served via a containerd registry mirror — pulls are **not** done from GitLab at runtime (`inbox-image-distribution.md:18-23`). First-party images are **tag-only** today (`digest: ""` appears 25× in `values.yaml`), and `latestMode=true` returns `latest`, forces `Always`, and *bypasses the digest* (`inbox-image-distribution.md:148-149`).

**(b) Gaps.**
- **No container-image CVE scanning anywhere in the pipeline.** `.rwx/docker.yml` has no `trivy`/`grype`/scan step (its only `*scan*` hits are `ssh-keyscan`). The only scanning that exists is in `.rwx/helm.yml:147-178` — kube-linter, kubescape, checkov, polaris — which lint *K8s manifests/posture*, not image layer CVEs. So a vulnerable base layer ships undetected.
- **Tag-only / `latest` refs** mean "what's deployed" is not pinned to a content hash, so a re-pull can silently change bits and `latestMode` defeats any pinned bundle.

**(c) Recommendations.**
- **Patch loop on the box:** patching = produce a fresh `<calver>` bundle in CI → land/seed it into `zot` → `helm upgrade`. Because the agent container is hardcoded `ImagePullPolicy: PullAlways` (`controller/internal/podmanager/manager.go:982`) — and litellm defaults to `Always` while the chart `gardenio.imagePullPolicy` helper otherwise defaults to `IfNotPresent` (verified against the rendered chart; the `~11 Deployments default to Always` figure in `inbox-image-distribution.md:43-44` overcounts) — a re-seed of an existing tag does NOT reliably roll the `IfNotPresent` pods to new bits — pin a new calver tag so the spec changes and pods actually restart.
- **Digest-pin first-party images** (close the `digest: ""` gap and force `latestMode=false` in single-box mode, `inbox-image-distribution.md:148-149`) so the deployed content is verifiable and a re-pull cannot drift.
- **Add image CVE scanning to the release pipeline** (Trivy/Grype as a gated `.rwx` task on every built image, and ideally Renovate for base-image/tool bumps). This is a genuine net-new control; the coverage/quality bots in the repo do not scan image CVEs.

### 4. Base-image currency (external pull surface)

**(a)/(b) Gap.** The gvisor-probe DaemonSet — itself the most privileged object in the chart (`hostNetwork: true`, `hostPID: true`, `privileged: true`, hostPath `/`) — defaults to `image: "public.ecr.aws/docker/library/ubuntu:22.04"` (`values.yaml:866`; rendered at `gvisor-namespace.yaml:161`). That is (1) an **external pull dependency** on `public.ecr.aws` (contradicting "zero external cloud deps" — it must be in the air-gap bundle, and `inbox-image-distribution.md:88` already flags it as a ref "no plan caught"), and (2) a **patch surface**: a stale `ubuntu:22.04` inside a privileged hostPID pod with `/` mounted is the worst place to carry an unpatched libc.

**(c) Recommendation.** Pin the probe image to a digest, pull it into the in-box bundle under its original `docker/library/ubuntu` path (so the containerd mirror serves it offline), and include it in the same monthly base-image refresh + CVE scan as the first-party images. Prefer a minimal base over full Ubuntu for the probe if `runsc`/`chroot` tooling allows.

### Patch posture table

| Layer | Mechanism | Cadence | Reboot impact |
|---|---|---|---|
| OS security pkgs (AL2023) | `dnf-automatic` `upgrade_type=security`, `apply_updates=yes` + timed reboot | Daily auto-apply | Reboot only in scheduled window |
| OS security pkgs (Ubuntu) | `unattended-upgrades` (security origin) + `needrestart -b` + `Automatic-Reboot 04:00` | Daily auto-apply | Reboot at 04:00 window |
| k3s | `INSTALL_K3S_VERSION` pin; manual installer bump or suspended `system-upgrade-controller` Plan | Monthly/quarterly, controlled | apiserver + containerd restart (planned); re-verify runsc template |
| First-party images | New `<calver>` bundle → seed `zot` → `helm upgrade`; digest-pinned | Per release / monthly | Pod restart only (no host reboot) |
| Base / mirror images (incl. gvisor-probe ubuntu) | Refresh in bundle + Trivy/Grype scan + Renovate base bumps | Monthly | Pod restart only |
| Image CVE scanning | **NEW** Trivy/Grype gated task in `.rwx/docker.yml` (does not exist today) | Every build | None |

**Net:** the only thing safe to fully automate unattended is *OS security packages + a scheduled reboot* (k3s recovers cleanly). k3s and image upgrades stay on a controlled cadence because each is a real, restart-inducing event on a non-HA box, and the privileged gvisor-probe's external `public.ecr.aws/ubuntu:22.04` is both an air-gap violation and a high-value patch target.

---

## Appendix: Verification Log

Every load-bearing factual claim in this report was adversarially re-checked against the actual repo by an independent agent (default-to-REFUTED if unsupported). Summary: of the claims checked, all were **CONFIRMED** except the six **PARTIAL** noted below (citation-line imprecisions or one overcount, corrected inline in the text). **Zero REFUTED.**

**Totals:** 60 claims checked — 55 CONFIRMED, 5 PARTIAL, 0 REFUTED.

### PARTIAL verdicts (and how they were handled)

- **[host-firewall]** _Postgres binds 5432 and NATS binds 4222/8222/8443/6222, all exposed only via ClusterIP Services_
  - Postgres: postgres/service.yaml:10 type ClusterIP, :12 port 5432 — correct. NATS listen config (nats/statefulset.yaml:16-17) is 0.0.0.0:4222 and http 0.0.0.0:8222 — correct. BUT the cited 'service ports 4222/8222/8443/6222 at lines 98-136, type ClusterIP line 96' conflates two Services. The main ClusterIP Service (type at line 96) exposes only 4222(:98), 8222(:102), and 8443(:107, gated on websocket.enabled). Port 6222 (cluster) appears only on the SEPARATE headless Service (clusterIP: None, lin

- **[host-firewall]** _No host-level firewall (nftables/ufw/firewalld) anywhere in repo today; only packet filter is AWS Security Groups in network terraform modul_
  - No-host-firewall part CONFIRMED: grep for nftables/ufw/firewalld in non-md files (.tf/.sh/.yaml/.yml) returns nothing; the only textual hit is the aspirational doc line 227. AWS SGs are the only packet filter (aws_security_group in terraform/modules/network/main.tf and modules/rds/main.tf). BUT the cited SG detail is wrong: at network/main.tf:211-234 the interface_endpoints SG ingress (lines 217-222) is 443/tcp from VPC CIDR + additional_cidr_blocks — NOT '0.0.0.0/0'. The 0.0.0.0/0 at line 228 i

- **[ssh]** _k3s ships and manages its own ingress/LB/firewall (Traefik, ServiceLB) and uses nftables/containerd, making firewall-backend consistency a r_
  - Repo-supported parts CONFIRMED: inbox-k8s-distro-choice.md:59 'Traefik ingress', :62 'ServiceLB / Klipper-LB built-in LoadBalancer', and pervasive 'vanilla embedded containerd' (lines 37, 97). UNSUPPORTED parts: the distro doc never mentions nftables, iptables, firewall, or fail2ban (grep returned zero matches), so 'uses nftables ... making firewall-backend consistency a real concern for fail2ban' is NOT evidenced in the repo docs. The 'hostPort-like daemonset' / firewall-backend-consistency fra

- **[k8s-api]** _The BYO profile runs auth OFF and uses ClusterIP + kubectl port-forward, and is a different (private-cluster) threat model, not the public-b_
  - Config facts confirmed at byo-cluster.md:48-49: Auth off (keycloak.enabled=false; key is keycloak.forwardAuth.enabled=false — claim wrote it as forwardAuth.enabled=false, a path difference) and Ingress 'off — ClusterIP + kubectl port-forward (tls.enabled=false, traefik.enabled=false, per-component ingress.enabled=false)'. However the 'different (private-cluster) threat model, not the public-box posture' framing is the claimant's interpretation: the doc actually calls BYO 'the public, reproducibl

- **[patching]** _Agent container hardcoded ImagePullPolicy PullAlways and ~11 Deployments default to Always. (inbox-image-distribution.md:43-44; manager.go:9_
  - Agent hardcode CONFIRMED: gardenio/controller/internal/podmanager/manager.go:982 'ImagePullPolicy: corev1.PullAlways'. The doc (inbox-image-distribution.md:43-44) does state '~11 first-party Deployments default to Always', so the citation is accurate to the doc text. However adversarial check of the chart REFUTES the underlying '~11 default Always': only litellm/deployment.yaml uses 'default "Always"' (4 occurrences, 1 file); the other 16 imagePullPolicy helper usages call gardenio.imagePullPolicy
