# Spec: Ingress (host-based HTTPS routing)

Status: draft for review
Roadmap item: 9 (new — added to host real LAN services)

## Problem

A smith service is reachable at `<nodeIP>:<NodePort>` (L4). To run real
frontends — forgejo at `git.kkjorsvik.com`, woodpecker at `ci.kkjorsvik.com`,
n8n at `n8n.kkjorsvik.com` — you need **host-based HTTPS routing**: something on
`:443` that terminates TLS with a valid cert and routes by `Host:` header to the
right service. smith has no such layer; it only provisions a cert for the
control plane's own domain.

Scope: **LAN-internal** services. No public exposure (the cluster is behind
NAT). Local DNS resolves the names to the cluster; clients are on the LAN.

## How it layers on what exists

```
browser (LAN) --https--> ingress (:443, TLS, Host routing)
                              |  http
                              v
                         service ClusterIP:port  (L4 LB, item 4)
                              |
                              v
                         replicas (cluster-routable IPs, items 1-3)
```

Ingress is a thin top layer: TLS + host routing → hand off to an existing
service's ClusterIP. The service's iptables LB already fans out to replicas
cluster-wide, so the ingress only has to terminate TLS and pick a backend by
hostname. Everything below it is done.

## Design overview (mirrors the service architecture)

```
Control plane                                  Agent (each node)
-------------                                  -----------------
Ingress store (host -> service)                ingress reverse proxy on :443
wildcard cert via DNS-01 (Route53)             - TLS via wildcard cert
GET /nodes/{id}/ingresses -> [{host,           - route Host -> ClusterIP:port
  clusterIP, port}]                            ingress-sync loop (pull rules)
GET /ingress/cert (mTLS) -> wildcard cert      cert-sync (pull wildcard cert)
```

An **agent-hosted** ingress: every node runs the proxy, so any node is a valid
entry point (pairs with a floating VIP for entry-HA). Rules and the cert are
pulled from the control plane exactly like routes and services are.

## Part 1 — Ingress resource (types + store + API)

```go
type Ingress struct {
    Host    string `json:"host"`    // "git.kkjorsvik.com" (unique key)
    Service string `json:"service"` // target service name
}
```

The target is a **service** (which already has a ClusterIP + Port), so the
ingress only needs host + service name. Persist in an `ingresses` table
(`host` PK, `service`), like services/subnets. Public auth'd API:
`POST /ingresses`, `GET /ingresses`, `DELETE /ingresses/{host}`.

## Part 2 — Wildcard cert (control plane)

Provision `*.kkjorsvik.com` via the **existing DNS-01 / Route53** path
(`internal/acme`, the same machinery as the control-plane cert — wildcards
*require* DNS-01, which we already use). One wildcard cert covers every
subdomain, so the ingress needs no SNI/per-host cert juggling. Cache it next to
the control-plane cert and renew on the same schedule.

Distribute over mTLS: `GET /ingress/cert` (internal mux) returns the wildcard
cert + key PEM. Agents fetch it **automatically** on startup and on a refresh
interval over the same mTLS channel they already use for registration / routes
/ services, and use it for TLS termination.

> No new per-node setup step: a new agent's bootstrap is unchanged — run
> `gencerts` for it and start it; it then pulls its subnet, routes, services,
> and now the wildcard cert on its own. The wildcard key never enters a manual
> deploy step.

> Security note: this puts the wildcard private key on every agent. On a
> trusted LAN homelab that's acceptable; for public/production workloads you'd
> want per-node certs or terminate centrally. Flagged, not solved, in v1.

## Part 3 — Ingress rule distribution (control plane)

`GET /nodes/{id}/ingresses` (internal mTLS) resolves each ingress's service to
its ClusterIP + Port and returns `[]{Host, ClusterIP, Port}` — the same
compute-and-distribute pattern as `/nodes/{id}/services`. Agents pull on the
heartbeat cadence. An ingress whose service doesn't exist (or has no ClusterIP)
is skipped.

## Part 4 — Agent ingress proxy

A new `runtime`/agent component:

- An `https://` server on **:443** (the agent runs as root, so the privileged
  port is fine), TLS config holding the wildcard cert.
- A handler that looks up `r.Host` in the current rule map and reverse-proxies
  to `http://<clusterIP>:<port>` via `httputil.ReverseProxy`. Backends speak
  plain HTTP inside the cluster (TLS terminates at the ingress).
  - `ReverseProxy` transparently handles WebSocket `Upgrade` and streaming —
    needed for n8n (websockets), woodpecker, and git's chunked transfers.
  - Unknown host → `502`/`404`.
- An optional `:80` listener that 301-redirects to `https://`.
- An ingress-sync loop pulls the rules each tick and atomically swaps the
  host→backend map under a lock (like route/service sync).
- Best-effort, like CNI/firewall: if `:443` can't bind or the cert is missing,
  the agent logs and runs without ingress.

## Part 5 — Entry point (deployment, not code)

LAN clients resolve the names via **your local DNS** (Pi-hole / router /
unbound) pointing `*.kkjorsvik.com` (or each name) at the cluster. Because the
ingress runs on every node, the target just needs to be a live node:

- **Floating VIP via keepalived/VRRP** (recommended): one stable IP that fails
  over between agent nodes; DNS points at it. Clean entry-HA. keepalived is
  external to smith (documented prerequisite; smith does not manage the VIP in
  v1).
- **DNS round-robin** across node IPs: zero extra infra, but resolver caching
  means a dead node lingers in rotation (browsers retry the next A record,
  which softens it).
- **Single node IP**: simplest; that node is a SPOF for entry (not for the
  balancing below it).

smith already load-balances across replicas internally, so the entry layer's
only job is "reach a live node" — which is why a VIP (or even RR) is enough.

## Sub-commits

1. **Ingress resource**: types, `ingresses` table + store, `POST/GET/DELETE
   /ingresses`. Inert to agents.
2. **Cert + distribution**: wildcard provisioning, `GET /ingress/cert`, rule
   computation + `GET /nodes/{id}/ingresses`.
3. **Agent ingress proxy**: :443 TLS reverse proxy + host routing + :80
   redirect + sync loops.

## Testing checklist

1. Run forgejo as a smith workload + service `forgejo` (ClusterIP). `POST
   /ingresses {host: git.kkjorsvik.com, service: forgejo}`.
2. Point LAN DNS `git.kkjorsvik.com` → node IP / VIP.
3. Browser `https://git.kkjorsvik.com` → forgejo UI, valid cert (wildcard), no
   warnings.
4. git clone over HTTPS and a websocket app (n8n) work through the proxy.
5. Kill the node holding the VIP → entry fails over; service still answers.
6. Delete the ingress → host stops routing; cert untouched.

## Decisions (locked 2026-06-23)

1. **Where the ingress runs**: **agent-hosted** on every node. Any node is a
   valid entry point (a floating VIP can fail over between them), and the
   wildcard cert is distributed automatically over the existing mTLS channel —
   new-agent setup adds no manual step.
2. **Implementation**: **native Go reverse proxy** (`httputil.ReverseProxy` +
   TLS) inside the agent. Self-contained; handles websockets/streaming.
3. **Cert**: **control-plane-provisioned wildcard** `*.kkjorsvik.com` via the
   existing DNS-01/Route53 path, distributed to agents over mTLS. One cert,
   no SNI/per-host juggling.
4. **Routing granularity**: **host-based only** for v1 (one service per
   hostname). Host + path-prefix routing is a planned v2 follow-on.

## Out of scope (v1) / future

- Path-prefix routing (v2).
- Public exposure (these are LAN-internal; public would go to a cloud
  provider per the homelab goal).
- smith-managed VIP/VRRP — keepalived stays an external prerequisite.
- Per-node certs / central termination — revisit if workloads ever need
  public/production exposure.
