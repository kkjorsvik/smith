# smith

A small, self-contained container orchestrator. A **control plane**
(`smith-server`) keeps a desired set of workloads running across a fleet of
**agent** nodes (`smith-agent`). Each agent owns the container lifecycle locally
via [containerd](https://containerd.io/); the control plane bin-packs replicas
onto nodes, continuously reconciles desired vs. actual state, rolls out spec
changes, fails replicas over when a node dies, and reports aggregated status.

It also handles the layers around the container: **cross-node networking**
(routable container IPs), **services** (ClusterIP/NodePort L4 load balancing),
**ingress** (host-based HTTPS), and a **web dashboard** — enough to host real
LAN services end to end.

All control-plane ↔ agent traffic is secured with **mutual TLS** against a
private CA. The public workload API is served over HTTPS with a Let's Encrypt
certificate obtained via the ACME **DNS-01** challenge (Route 53).

---

## Contents

- [Features](#features)
- [Architecture](#architecture)
- [Ports](#ports)
- [Prerequisites](#prerequisites)
- [Build](#build)
- [mTLS: generating certificates](#mtls-generating-certificates)
- [Filesystem layout & fixed values](#filesystem-layout--fixed-values)
- [Running the control plane](#running-the-control-plane)
- [Running an agent](#running-an-agent)
- [Provisioning fresh nodes (scripts)](#provisioning-fresh-nodes-scripts)
- [Workloads](#workloads)
- [API reference](#api-reference)
- [How it works](#how-it-works)
- [Operational notes & caveats](#operational-notes--caveats)
- [Project layout](#project-layout)
- [Development](#development)

---

## Features

- **Resource-aware bin-packing scheduler** — replicas are placed best-fit on
  nodes with enough free CPU/memory (15% reserved for the system), spread across
  nodes by anti-affinity. A replica that doesn't fit stays pending.
- **Multi-replica workloads** — a workload declares `replicas`; the control
  plane expands it into N instances (`<id>-0`, `<id>-1`, …) and spreads them.
- **Rolling updates** — changing a workload's spec (image/args/env/ports/
  resources) rolls replicas in place, rate-limited by `max_unavailable`. Hash of
  the container-defining fields decides what's stale.
- **Cross-node container networking** — each node gets a unique `/24` from a
  cluster CIDR (`10.22.0.0/16`); container IPs are routable cluster-wide via
  per-node bridges (CNI) + static routes, with selective iptables masquerade.
- **Services (L4 load balancing)** — a service gets a stable **ClusterIP** and a
  **NodePort** and load-balances across a workload's running replica IPs via
  iptables on every node (kube-proxy style).
- **Ingress (host-based HTTPS)** — map a hostname to a service; every agent runs
  a TLS-terminating reverse proxy on `:443` that routes by `Host:` using a
  control-plane-provisioned wildcard cert.
- **Stateful workloads** — persistent **NFS-backed volumes** bind-mounted into a
  container; data survives recreation, rolling updates, and node failover.
  Volume-bearing workloads are single-replica (single writer).
- **Failover** — when a node misses its heartbeat window it is declared dead and
  its replicas are rescheduled onto healthy nodes.
- **Persistent desired state** — workloads, services, ingresses, and per-node
  subnet allocations live in SQLite, surviving a control-plane restart.
- **Cluster-wide status & logs** — `GET /status` aggregates real per-replica
  state (status + PID + IP) from every alive agent; `GET /workloads/{id}/logs`
  streams a replica's captured stdout/stderr.
- **Web dashboard** — an embedded UI at `/` shows nodes, workloads/replicas,
  services, and per-replica logs.
- **End-to-end TLS** — mutual TLS on the internal plane (private CA); public
  HTTPS via Let's Encrypt DNS-01.
- **Authenticated public API** — the public `:443` endpoints require a bearer
  token; the internal `:9443` plane is authenticated by agent client certs.
- **Health checks** — workloads may declare `http` or `exec` probes (see
  [caveats](#operational-notes--caveats) for current wiring status).

---

## Architecture

```
                      public HTTPS (:443, Let's Encrypt via DNS-01)
                                     │
                           ┌─────────▼──────────┐
   workload API  ────────► │   control plane    │
   (workloads, status,     │   (smith-server)   │
    nodes, assignments)    └─────────┬──────────┘
                          internal mTLS (:9443)
                 ┌───────────────────┼────────────────────┐
        assign / unassign /     register / heartbeat   assign / unassign /
        status  (mTLS, ►agent)   (mTLS, agent►)        status  (mTLS, ►agent)
                 │                                          │
         ┌───────▼────────┐                        ┌────────▼───────┐
         │  smith-agent   │ ── containerd          │  smith-agent   │ ── containerd
         │ (:9000, mTLS)  │      ►                 │ (:9000, mTLS)  │      ►
         └────────────────┘                        └────────────────┘
```

Two trust boundaries:

- **Public API** (`:443`) — the workload-management API, served over HTTPS with
  a publicly-trusted **Let's Encrypt** certificate obtained via the ACME
  **DNS-01** challenge using Route 53. DNS-01 is used because the server is
  typically behind NAT where port 80 is unreachable, so HTTP-01 cannot be used.
  Callers authenticate with a **bearer token** (`Authorization: Bearer <token>`)
  loaded from `/etc/smith/token`.
- **Control plane ↔ agents** — every internal connection (the control plane's
  internal API on `:9443`, and each agent's API on its `-addr`) runs over
  **mutual TLS** using a private, self-signed Smith CA. Both ends present and
  verify a CA-signed certificate (TLS 1.3, `RequireAndVerifyClientCert`);
  connections without a valid client cert are rejected.

### Components

| Binary | Role |
|--------|------|
| `smith-server` | Control plane: serves the public + internal APIs, runs the bin-packing scheduler and reconcile loop, allocates per-node subnets / ClusterIPs / NodePorts, provisions certs, and persists desired state. Also provides the `gencerts` and `add-agent` subcommands. |
| `smith-agent` | Worker: registers (receiving its container subnet), heartbeats, and serves an mTLS API to receive assignments and report replica status/logs. Manages containers via local containerd, programs its CNI bridge + cross-node routes + service load-balancing (iptables), and runs the ingress reverse proxy on `:80`/`:443`. |

Beyond the control loop shown above, each agent also: terminates ingress TLS on
`:443` (HTTP `:80` redirects to it) and reverse-proxies by host to a service
ClusterIP; and programs iptables so a service's ClusterIP/NodePort load-balances
across replica IPs cluster-wide. The control plane distributes the routing
table, service endpoints, ingress rules, and the wildcard cert to agents over
the same internal mTLS channel used for register/heartbeat.

---

## Ports

| Component | Port | Protocol | Purpose |
|-----------|------|----------|---------|
| smith-server | `:443` | HTTPS (Let's Encrypt) | Public workload API + web dashboard |
| smith-server | `:9443` | mTLS (Smith CA) | Internal API: register/heartbeat, route/service/ingress distribution, cert |
| smith-agent | `:9000`\* | mTLS (Smith CA) | Receives assign/unassign/status/logs from the control plane |
| smith-agent | `:443` | HTTPS (wildcard cert) | Ingress: TLS termination + host-based reverse proxy |
| smith-agent | `:80` | HTTP | Ingress: redirect to `:443` |
| smith-agent | NodePort | TCP/UDP | Per-service NodePorts (`30000–32767`) exposed on every node |

\* Whatever host:port you pass to the agent's `-addr` flag. The ingress
listeners are best-effort — the agent logs and runs without them if `:80`/`:443`
can't bind or no wildcard cert is available yet.

---

## Prerequisites

- **Go 1.26+** (module targets `go 1.26.4`).
- **containerd** running on every host (control plane and agents), reachable at
  its default socket. The server and agent connect to the local containerd on
  startup and clean up the `smith` namespace.
- **CNI plugins** (`bridge`, `host-local`, `portmap` from
  [containernetworking/plugins](https://github.com/containernetworking/plugins))
  in `/opt/cni/bin` on **agent** hosts — the per-node bridge network needs them.
  The control plane runs no workloads and needs none. (The agent setup script
  installs these for you; see [Provisioning](#provisioning-fresh-nodes-scripts).)
- **`nfs-common`** on agent hosts **if** you run stateful workloads — provides
  `mount.nfs` for [NFS-backed volumes](#stateful-workloads). Also installed by
  the agent setup script.
- **Root / privileged access** on each host — binding `:443`/`:80`/`:9443`,
  talking to the containerd socket, and writing `/etc/smith` and `/var/lib/smith`
  generally require it.
- **AWS credentials** (control plane only, for the public `:443` cert) resolvable
  via the default chain — env vars, `~/.aws/...`, or an instance role — with
  Route 53 permissions (see [Running the control plane](#running-the-control-plane)).

---

## Build

```bash
go build -o bin/smith-server ./cmd/server
go build -o bin/smith-agent  ./cmd/agent
```

`bin/` is git-ignored. The server binary embeds the AWS SDK (for Route 53), so
it is the larger of the two.

---

## mTLS: generating certificates

All internal communication requires certificates signed by a shared **Smith CA**.
The server binary includes a `gencerts` subcommand that creates the CA, the
server certificate, and one certificate per agent host in a single step.

```bash
sudo ./bin/smith-server gencerts \
  -out   /etc/smith/certs \
  -hosts smith-agent-01.kkjorsvik.com,smith-agent-02.kkjorsvik.com
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-out` | `/etc/smith/certs` | Directory to write certs into (created `0700`) |
| `-hosts` | _(empty)_ | Comma-separated agent hostnames/IPs to issue certs for |

This writes into `-out`:

| File | Purpose |
|------|---------|
| `ca.crt`, `ca.key` | The Smith CA (10-year validity). **`ca.key` must never leave the host that generated it.** |
| `server.crt`, `server.key` | Control-plane identity, CN/SAN `smith-server-01.kkjorsvik.com` (2-year validity) |
| `<label>.crt`, `<label>.key` | One pair per `-hosts` entry, named from the first hostname label — e.g. `smith-agent-01.kkjorsvik.com` → `smith-agent-01.crt` / `smith-agent-01.key` |

> **`gencerts` reuses an existing CA.** If `ca.crt`/`ca.key` are already present
> in `-out`, it loads and keeps them, only (re)issuing the server + listed agent
> leaves. So re-running it to add a host is safe — it never re-keys the cluster.
> Pass `-force-ca` to deliberately regenerate the CA (destructive: invalidates
> every existing cert). Private keys are written `0600`.
>
> To add an agent **after** the cluster is up, prefer `add-agent` (below) — it
> issues one leaf against the existing CA and packages a ready-to-deploy bundle.

Every leaf certificate carries **both** `clientAuth` and `serverAuth` extended
key usages, so each component uses its single cert for both inbound serving and
outbound calls. Keys are P-256 ECDSA.

> **Important — certificate SANs must match the addresses you dial.**
> mTLS verifies the peer's certificate against the hostname used to reach it.
> Issue agent certs for the **hostnames** you will actually connect to (the value
> you pass as the agent's `-addr`), not bare IPs. The server certificate is
> issued for `smith-server-01.kkjorsvik.com`, so agents must use that exact
> hostname in `-server`. A mismatch produces TLS "certificate is valid for X, not
> Y" handshake errors at runtime.

### Distributing certificates

| Host | Needs |
|------|-------|
| Control plane | `ca.crt`, `server.crt`, `server.key` |
| Each agent | `ca.crt`, its own `<label>.crt`, its own `<label>.key` |

Distribute `ca.key` to **no host** — keep it only where you run `gencerts`. By
default everything is read from `/etc/smith/certs/` (see fixed paths below).

---

## Filesystem layout & fixed values

Several paths and names are currently **hardcoded** (not configurable via flags
on the server). Plan your deployment around them, or change them in source:

| Value | Where | Notes |
|-------|-------|-------|
| `/etc/smith/certs/{ca,server}.{crt,key}` | server startup | Internal mTLS material (read on boot) |
| `/etc/smith/token` | server startup | Public API bearer token (read on boot; server refuses to start if missing/empty) |
| `/var/lib/smith/state.db` | server | SQLite store: workloads, services, ingresses, subnet allocations |
| `/var/lib/smith/autocert/server.{crt,key}` | server | Cached public Let's Encrypt cert (control-plane domain) |
| `/var/lib/smith/autocert/wildcard.{crt,key}` | server | Cached `*.kkjorsvik.com` wildcard cert (ingress; shipped to agents) |
| `smith-server-01.kkjorsvik.com` | server cert + public ACME domain | The public hostname; also the server cert CN |
| `*.kkjorsvik.com` / `kkjorsvik.com` | wildcard cert domain / Route 53 zone | Ingress wildcard, provisioned via DNS-01 |
| `10.22.0.0/16` | container network (`BridgeSubnet`) | Cluster pod CIDR, carved into per-node `/24`s |
| `10.23.0.0/16` | service network (`ServiceCIDR`) | ClusterIP pool |
| `30000–32767` | NodePort range | Per-service host ports |
| `:443`, `:80`-free, `:9443` | server | Public HTTPS / internal mTLS (server uses no `:80` — DNS-01 needs no HTTP listener) |
| `:443`, `:80` | agent | Ingress proxy (TLS) + redirect — must be free on agent hosts |
| `5s` reconcile interval | server | How often desired/actual state is reconciled |
| `10s` heartbeat interval | agent | How often an agent pings the control plane (also the route/service/ingress refresh cadence) |
| `30s` heartbeat timeout | control plane | A node missing this long is declared dead |

---

## Running the control plane

```bash
sudo ./bin/smith-server
```

On startup it:

1. Connects to the local containerd and cleans up the `smith` namespace.
2. Opens the SQLite store at `/var/lib/smith/state.db`.
3. Loads internal mTLS material from `/etc/smith/certs/`.
4. Loads the public API bearer token from `/etc/smith/token` — **the server
   refuses to start if it is missing or empty.**
5. Starts the reconcile loop (every 5s).
6. Serves the internal mTLS API on `:9443` and provisions/serves the public
   HTTPS API on `:443`.

**Create the API token first.** The public `:443` endpoints require a bearer
token (the internal `:9443` plane is protected by mTLS instead and needs no
token). Generate one and write it to `/etc/smith/token` before starting:

```bash
# Generate a random token and write it to the token file
openssl rand -hex 32 | sudo tee /etc/smith/token
sudo chmod 600 /etc/smith/token
```

The token is the entire file contents trimmed of surrounding whitespace (the
trailing newline from `tee` is ignored). To rotate it, replace the file and
restart the server.

**Public certificate (`:443`) prerequisites.** The DNS-01 flow needs AWS
credentials resolvable via the default chain, with permission to:

- `route53:ListHostedZonesByName`
- `route53:ChangeResourceRecordSets`

on the hosted zone for `smith-server-01.kkjorsvik.com`. The hosted-zone ID is
discovered automatically; a `_acme-challenge` TXT record is created, validated,
then removed. The issued cert is cached under `/var/lib/smith/autocert/` and
reused on restart.

> The internal mTLS plane on `:9443` is **independent** of the public cert —
> control plane ↔ agent traffic works even if public cert issuance fails (e.g.
> no AWS creds on a dev box).

---

## Running an agent

`-cert` and `-key` are required; `-ca` defaults to `/etc/smith/certs/ca.crt`.

```bash
sudo ./bin/smith-agent \
  -id     smith-agent-01 \
  -addr   smith-agent-01.kkjorsvik.com:9000 \
  -server smith-server-01.kkjorsvik.com:9443 \
  -ca     /etc/smith/certs/ca.crt \
  -cert   /etc/smith/certs/smith-agent-01.crt \
  -key    /etc/smith/certs/smith-agent-01.key
```

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `-id` | yes | — | Unique node ID (e.g. `smith-agent-01`) |
| `-addr` | yes | — | This agent's mTLS address the control plane calls back to (must match the cert SAN) |
| `-server` | yes | — | Control plane's internal mTLS address, `<host>:9443` |
| `-ca` | no | `/etc/smith/certs/ca.crt` | Smith CA cert used to verify peers |
| `-cert` | yes | — | This agent's certificate |
| `-key` | yes | — | This agent's private key |

On startup the agent connects to its local containerd (cleaning up the `smith`
namespace), registers with the control plane over mTLS, starts a 10s heartbeat
loop, and serves its own mTLS API so the control plane can push assignments and
query container status.

---

## Provisioning fresh nodes (scripts)

`scripts/` and the `add-agent` subcommand automate standing up boxes on a fresh
Ubuntu server (`apt` + systemd). Prerequisites become automatic: the server
script installs containerd; the agent script installs containerd **and** the
pinned CNI plugins into `/opt/cni/bin`.

### Control plane

Build `smith-server`, then on the control-plane box:

```bash
sudo ./scripts/setup-server.sh bin/smith-server
```

It installs containerd, lays out `/etc/smith` + `/var/lib/smith`, generates
`/etc/smith/token`, bootstraps the CA + server cert (via `gencerts`), installs
`bin/` to `/usr/local/bin`, and enables `smith-server.service`. Re-running is a
no-op (token + CA preserved). Provide AWS creds for the public `:443` cert via
`/etc/smith/server.env`, an instance role, or `~/.aws` (see the script's
closing notes), then `systemctl restart smith-server`.

### Adding an agent

Run **on the control plane** (where `ca.key` lives), with the agent binary built:

```bash
sudo ./bin/smith-server add-agent \
  -host   smith-agent-03.kkjorsvik.com \
  -binary bin/smith-agent
# writes ./smith-agent-03.tar.gz
```

| Flag | Default | Description |
|------|---------|-------------|
| `-host` | _(required)_ | Agent hostname / cert SAN |
| `-id` | first label of `-host` | Node id (`smith-agent-03`) |
| `-addr` | `<host>:9000` | mTLS callback address |
| `-server` | `smith-server-01.kkjorsvik.com:9443` | Control-plane internal address |
| `-out` | `/etc/smith/certs` | Cert dir — **must already contain the CA** |
| `-binary` | `bin/smith-agent` | Agent binary to embed in the bundle |
| `-bundle` | `./<id>.tar.gz` | Output tarball path |
| `-nfs` | _(empty)_ | Cluster NFS share for stateful volumes, e.g. `unraid.kkjorsvik.com:/mnt/user/smith` (sets `SMITH_NFS_SOURCE`) |

`add-agent` issues one leaf against the existing CA (never regenerates it),
verifies it chains, and packages a self-contained tarball: the agent's
`ca.crt` + leaf cert/key, an `agent.env`, the setup script, the systemd unit,
and the `smith-agent` binary. **`ca.key` is never bundled.** Then on the new box:

```bash
scp smith-agent-03.tar.gz newbox:~/
ssh newbox 'tar xzf smith-agent-03.tar.gz && cd smith-agent-03 && sudo ./setup.sh'
```

`setup.sh` installs containerd + CNI plugins, drops the certs/binary/unit into
place, and starts `smith-agent.service`. The node then pulls its subnet, routes,
services, ingress rules, and the wildcard cert from the control plane on its own
— no further per-node configuration.

### Updating a running cluster

For a **code update** (new binaries, certs unchanged), don't re-bundle — just
replace the binaries and restart the services. `scripts/update.sh` does this as
a rolling update; run it on the control-plane box with your agent hosts:

```bash
./scripts/update.sh smith-agent-01.kkjorsvik.com smith-agent-02.kkjorsvik.com
```

It records each node's current replica count, builds both binaries from the
current checkout (`git pull` first if you want newer code), updates + restarts
the control plane, then rolls each agent **one at a time** — pushing the new
binary, restarting `smith-agent`, and waiting for the node to re-register (which
happens at agent startup) and its replicas to return to running before moving to
the next. It does **not** wait for the running agents to self-heal the
control-plane restart first — the per-agent restart re-registers each node
regardless, so this also works when the agents are still on an older binary.
Each agent's containers cycle briefly as it restarts, so spread a workload's
`replicas` across nodes (and front it with a service) to stay up during the roll.

Run it as your normal user (it `sudo`s the privileged local steps); starting it
with `sudo` works too — either way `ssh`/`scp` use *your* keys, since root on the
control plane has none for the agents. Requirements: `jq`, ssh access to the
agents, and that your user can `sudo` **non-interactively** (NOPASSWD) on the
agents — the remote binary swap runs `sudo` over a non-interactive ssh.

---

## Workloads

A workload describes a container the cluster should keep running:

```json
{
  "id": "web",
  "image": "docker.io/library/nginx:latest",
  "args": [],
  "replicas": 3,
  "max_unavailable": 1,
  "env": { "TZ": "UTC" },
  "ports": [{ "host_port": 8080, "container_port": 80, "protocol": "tcp" }],
  "resources": { "cpu_millicores": 500, "memory_mb": 256 },
  "health_check": {
    "type": "http",
    "url": "http://localhost:8080/healthz",
    "initial_delay": "5s",
    "interval": "10s",
    "threshold": 3
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique workload ID. Replicas get `<id>-0`, `<id>-1`, … container IDs |
| `image` | string | Fully-qualified image ref (pulled if not present locally) |
| `args` | string[] | Command arguments |
| `replicas` | int? | Instances to run, spread across nodes (default 1) |
| `max_unavailable` | int? | Replicas that may be down at once during a rolling update (default 1) |
| `env` | map? | Environment variables set in the container |
| `ports` | object[]? | Host→container port mappings (`host_port`, `container_port`, `protocol`) published via the portmap CNI plugin |
| `resources` | object? | `cpu_millicores` (1000 = 1 core) and `memory_mb` limits; also used as the scheduler's bin-packing request. `memory_mb` is enforced (OOM-kill) |
| `volumes` | object[]? | Persistent NFS-backed mounts (`name`, `path`). Makes the workload stateful — see [Stateful workloads](#stateful-workloads). Limited to 1 replica |
| `health_check` | object? | Optional probe (see below) |

Changing `image`, `args`, `env`, `ports`, or `resources` triggers a **rolling
update** (replicas recreated in place, at most `max_unavailable` down at once).
Changing only `replicas` scales out/in without a roll.

`health_check` (optional):

| Field | Type | Description |
|-------|------|-------------|
| `type` | `"http"` \| `"exec"` | HTTP GET expecting 2xx, or a command expecting exit code 0 |
| `url` | string | For `http` probes |
| `command` | string[] | For `exec` probes (run inside the container) |
| `initial_delay` | duration | Wait before the first probe, e.g. `"5s"` |
| `interval` | duration | Time between probes, e.g. `"10s"` |
| `threshold` | int | Consecutive failures before marking unhealthy |

Durations are human-readable strings (`"5s"`, `"1m30s"`), not nanoseconds.

### Services

A service is a stable L4 endpoint that load-balances across a workload's running
replicas. The control plane assigns a **ClusterIP** (from `10.23.0.0/16`) and a
**NodePort** (`30000–32767`); every agent programs iptables so the ClusterIP is
reachable cluster-wide and the NodePort is reachable on every node's host IP.

```bash
curl https://smith-server-01.kkjorsvik.com/services \
  -H "Authorization: Bearer $(sudo cat /etc/smith/token)" \
  -H 'Content-Type: application/json' \
  -d '{"name":"web","workload_id":"web","port":80,"target_port":80}'
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique service name (also the DELETE key) |
| `workload_id` | string | Workload whose replicas back the service |
| `port` | int | Port clients hit on the ClusterIP |
| `target_port` | int | Container port traffic is forwarded to |
| `protocol` | string? | `tcp` (default) or `udp` |
| `cluster_ip`, `node_port` | — | Assigned by the control plane (response only) |

### Ingress

An ingress maps a hostname to a service for host-based HTTPS. Every agent's
`:443` proxy terminates TLS with the wildcard cert and reverse-proxies the
matching `Host:` to the service's ClusterIP. Point LAN DNS for the hostname at a
live agent (or a floating VIP across them).

```bash
curl https://smith-server-01.kkjorsvik.com/ingresses \
  -H "Authorization: Bearer $(sudo cat /etc/smith/token)" \
  -H 'Content-Type: application/json' \
  -d '{"host":"git.kkjorsvik.com","service":"forgejo"}'
```

| Field | Type | Description |
|-------|------|-------------|
| `host` | string | FQDN to route (unique key; covered by `*.kkjorsvik.com`) |
| `service` | string | Target service name |

### Stateful workloads

A workload with `volumes` is **stateful**: each volume is a directory on a
cluster **NFS share** bind-mounted into the container. Because the share is
reachable from every node, the data survives container recreation, rolling
updates, agent restarts, and **node failover** — when a node dies the single
replica reschedules elsewhere and mounts the same data. To guarantee a single
writer, a volume-bearing workload is limited to **`replicas: 1`** (the API
rejects more).

Configure the share once per agent via `SMITH_NFS_SOURCE` (see
[`add-agent -nfs`](#adding-an-agent)); smith stores each volume at
`<share>/<workloadID>/<name>`. Agents need `nfs-common` (the setup script
installs it). Volume data is **never auto-deleted** when a workload is removed.

**NFS export requirements.** The share's export must:
- **Allow the agent hosts** (e.g. an Unraid NFS Rule like
  `192.168.1.0/24(sec=sys,rw,no_root_squash)` — or `*` to test). Host-based, no
  user/password.
- Use **`no_root_squash`**. Images like Postgres run their entrypoint as root and
  `chown` their data dir; with root squashed that `chown` fails
  (`Operation not permitted`) and the container crash-loops. smith creates each
  volume dir **world-writable (0777)** so a container that drops to a non-root
  uid (postgres=999, etc.) can still write — the app manages the perms of its own
  data dir inside.

> Tip: changing an NFS export rule while agents have the share mounted strands
> the mount (`Stale file handle`). After editing the export, on each agent:
> `sudo umount -f -l /var/lib/smith/nfs && sudo systemctl restart smith-agent`.

**Sharing one Postgres across services.** Run a single `postgres` workload with a
superuser password, expose it as a service (stable ClusterIP), and give each app
its own database + role — added live, with no change to the workload.
`scripts/provision-db.sh <app>` does that (looks up the service NodePort and runs
`CREATE DATABASE`/`CREATE ROLE`/`GRANT`, generating a password):

```bash
./scripts/provision-db.sh forgejo
# -> database/user "forgejo" + a generated password; point the app's DB env at
#    the postgres ClusterIP. Re-run per new service (woodpecker, n8n, …).
```
Each app connects to the same `<clusterip>:5432` with its own `NAME`/`USER`/`PASSWD`.

```json
{
  "id": "postgres",
  "image": "docker.io/library/postgres:16",
  "env": { "POSTGRES_PASSWORD": "..." },
  "replicas": 1,
  "resources": { "memory_mb": 512 },
  "volumes": [{ "name": "data", "path": "/var/lib/postgresql/data" }]
}
```

| Volume field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique within the workload; the NFS subdir name. Must match `[a-z0-9-]+` |
| `path` | string | Absolute mount path inside the container |

Front it with a **service** (ClusterIP) so other workloads reach it by a stable
address — no ingress (it isn't HTTP).

> **Caveats.** A network partition can briefly produce two writers (the old
> replica keeps running while a new one starts after the 30s dead-timeout);
> Postgres's lock file is unreliable over NFS, so treat backups as the real
> durability net. Fencing/STONITH is out of scope. See
> `docs/specs/stateful-volumes.md`.

---

## API reference

### Public API — `:443` (HTTPS)

**All public endpoints require bearer token authentication.** Send the token
from `/etc/smith/token` on every request:

```
Authorization: Bearer <token>
```

Requests with a missing/malformed header or a non-matching token get
`401 Unauthorized`. (The token is compared in constant time.)

The dashboard at `GET /` and `GET /ui` is served **unauthenticated** (it's just
the static shell; it asks for the token in-browser and stores it locally, then
calls the authenticated endpoints below).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/{$}`, `/ui` | Web dashboard (unauthenticated shell) |
| `GET` | `/workloads` | List desired workloads |
| `POST` | `/workloads` | Add/update a workload (JSON body as above) |
| `DELETE` | `/workloads/{id}` | Remove a workload (reconciler stops its replicas next tick) |
| `GET` | `/workloads/{id}/logs` | Stream a replica's captured stdout/stderr (proxied from its agent) |
| `GET` | `/status` | Cluster-wide replica status, aggregated from every alive agent |
| `GET` | `/nodes` | Registered nodes and their last heartbeat |
| `DELETE` | `/nodes/{id}` | Deregister a node |
| `GET` | `/assignments` | Current replica → node assignments |
| `POST` `GET` `DELETE` | `/services`, `/services/{name}` | Manage services (L4 LB) |
| `POST` `GET` `DELETE` | `/ingresses`, `/ingresses/{host}` | Manage ingresses (host HTTPS) |

### Internal API — `:9443` (mTLS, used by agents)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/nodes/register` | Agent registration; response carries the node's assigned subnet/gateway |
| `POST` | `/nodes/{id}/heartbeat` | Agent heartbeat |
| `GET` | `/nodes/{id}/routes` | This node's cross-node container routing table (peer subnet → host IP) |
| `GET` | `/nodes/{id}/services` | Resolved service endpoints (ClusterIP/NodePort + running replica IPs) |
| `GET` | `/nodes/{id}/ingresses` | Resolved ingress rules (host → ClusterIP:port) |
| `GET` | `/ingress/cert` | The wildcard cert + key bundle for ingress TLS |

### Agent API — agent `-addr` (mTLS, used by the control plane)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/assign` | Start/keep a replica's container |
| `DELETE` | `/assign/{id}` | Stop a replica's container |
| `GET` | `/status` | This node's observed container state (status + PID + IP) |
| `GET` | `/logs/{id}` | This replica's captured logs |

### Examples

Add a workload (note the bearer token):

```bash
curl https://smith-server-01.kkjorsvik.com/workloads \
  -H "Authorization: Bearer $(sudo cat /etc/smith/token)" \
  -H 'Content-Type: application/json' \
  -d '{"id":"web","image":"docker.io/library/nginx:latest","args":[]}'
```

`GET /status` returns observed state aggregated across agents, keyed by node ID,
then replica (container) ID:

```json
{
  "smith-agent-01": {
    "web-0": { "id": "web-0", "status": "running", "pid": 12345, "ip": "10.22.1.7" }
  },
  "smith-agent-02": {
    "web-1": { "id": "web-1", "status": "running", "pid": 9876, "ip": "10.22.2.4" }
  }
}
```

A node with no reported replicas (or one that was unreachable this cycle)
appears with an empty object rather than being omitted. Container `status`
values come from containerd: `running`, `created`, `stopped`, `paused`,
`unknown`. `ip` is the CNI-assigned container address (empty if networking is
disabled).

---

## How it works

**Registration & heartbeats.** An agent `POST`s to `/nodes/register` on startup,
reporting its CPU count and total memory; the response carries the node's
assigned container subnet/gateway (allocated from `10.22.0.0/16` and persisted,
so it's stable across restarts). The agent then heartbeats every **10s** and, on
the same cadence, pulls its routing table, service endpoints, ingress rules, and
the wildcard cert. The control plane marks a node **dead** after **30s**
(`HeartbeatTimeout`).

**Replicas.** Each tick the reconciler **expands** every desired workload into
`replicas` instances (`<id>-0` … `<id>-N`), and the rest of the loop operates on
those instances.

**Scheduling (bin-packing).** A replica only lands on a node whose *schedulable*
capacity (CPU/memory minus a **15%** system reserve) still fits its
`resources` request. Among fitting nodes the scheduler picks fewest siblings
first (anti-affinity spread), then **best-fit** (least remaining capacity) as a
tiebreak. A replica with no fitting node stays **pending** until capacity frees
up. Placement is sticky — an assigned replica keeps its node until the node dies
or the replica is removed.

**Reconcile loop (every 5s).** Each tick the control plane:

1. **Evicts dead nodes** — replicas on a dead node are unassigned (push records
   cleared) so they reschedule; the node is removed.
2. **Assigns & pushes** — each desired replica is assigned a node. A push to the
   agent happens when the replica is **new/moved**, or when its **spec hash is
   stale** (a rolling update) — and a roll only proceeds if the replica is
   currently running and the parent's in-flight unavailable count is below
   `max_unavailable`. Up-to-date replicas are left alone (no push every tick).
3. **Verifies running state** — for each pushed replica past a `2 × interval`
   (**10s**) grace period, it checks the agent's `/status`; if the container
   isn't `running`, the push record is cleared so it re-pushes next tick.
4. **Stops removed replicas** — anything assigned but no longer desired (a
   deleted workload, or a scaled-in replica) is unassigned and stopped.

**Spec hash & rolling updates.** `specHash` digests the container-defining
fields (image/args/env/ports/resources). When it changes, replicas are stale and
get recreated in place, rate-limited by `max_unavailable`. Changing only
`replicas` doesn't change the hash, so scaling never triggers a roll.

**Networking.** Each node builds a CNI bridge for its `/24` (host-local IPAM,
selective masquerade) and installs static routes to every peer's subnet via the
peer's underlay host IP — so a container on one node reaches a container on
another by its real IP. The control plane computes each node's routing table
from the live node set and hands it back via `/nodes/{id}/routes`.

**Services & ingress.** A service's resolved endpoints (ClusterIP, NodePort,
running replica IPs) are distributed to every agent, which programs iptables
(kube-proxy style: per-service DNAT chains with random replica selection,
conntrack pinning, hairpin/NodePort masquerade). Ingress rules (host →
ClusterIP:port) and the wildcard cert are distributed the same way; each agent's
`:443` proxy uses them to terminate TLS and route by `Host:`.

**Status aggregation.** `GET /status` calls `Reconciler.AggregateStatus`, which
fans out to every alive agent's `/status` over mTLS and merges by node. (The
control plane runs no workloads — they're all on agents — so it aggregates
rather than reading its own containerd.)

**Failover, end to end.** Node dies → misses heartbeats → declared dead at 30s →
its replicas unassigned → rescheduled (bin-packed) onto surviving nodes → pushed
→ verified running. Desired state and subnet allocations are in SQLite, so this
holds across a control-plane restart.

**Control-plane restart.** The node registry and assignments are **in-memory**,
so a restarted control plane comes back not knowing any nodes. Agents' containers
keep running throughout (the agent owns them locally). When an agent's next
heartbeat returns `404` (unknown node), it **re-registers** automatically —
getting its same persisted subnet back — and the control plane re-learns the
node and re-pushes assignments. So the cluster self-heals within ~one heartbeat
interval after a server restart, with no agent intervention.

---

## Operational notes & caveats

- **Hostnames, not IPs.** Because of mTLS SAN verification, agents must be
  addressed by the hostnames their certs were issued for. See the warning in
  [cert generation](#mtls-generating-certificates).
- **Hardcoded names/paths.** `smith-server-01.kkjorsvik.com`, `/etc/smith`,
  `/var/lib/smith`, and the intervals are fixed in source today; adjust there if
  your environment differs.
- **Health checks are defined but not yet wired into reconciliation.** Workloads
  accept a `health_check`, and the control plane has a `health.Monitor` that can
  run `http`/`exec` probes, but the reconcile loop does not currently start
  watchers or act on health results. Probes also run from the control plane's
  local containerd, so `exec` probes won't find agent-hosted containers as-is —
  treat health checks as forward-looking until wired up.
- **One cert per identity.** Each agent's cert is used for both serving (inbound
  from the control plane) and calling out (register/heartbeat). The control
  plane's `server.crt` is likewise used for both its `:9443` listener and its
  outbound calls to agents.
- **The wildcard key is on every agent.** Ingress TLS terminates on each node,
  so the control plane ships the `*.kkjorsvik.com` private key to every agent
  over mTLS. Acceptable for a trusted LAN homelab; for public/production you'd
  want per-node certs or central termination. Flagged, not solved.
- **Agents bind `:80`/`:443`.** The ingress proxy needs them free on every agent
  host. If they're taken (or no wildcard cert exists yet) the agent logs and
  runs without ingress — scheduling/services are unaffected.
- **Entry-point HA is external.** smith load-balances across replicas, but
  getting LAN clients to *a live agent* (a floating VIP via keepalived, or DNS
  round-robin) is left to you — smith doesn't manage a VIP.

---

## Project layout

```
cmd/
  server/        smith-server entrypoint (+ gencerts / add-agent subcommands)
  agent/         smith-agent entrypoint
internal/
  acme/          Route 53 DNS-01 ACME solver (control-plane + wildcard certs)
  agent/         agent: register, heartbeat, manage containers, ingress proxy
  api/           control-plane HTTP APIs (public :443, internal :9443) + cert provisioning
  health/        health-check Monitor (http/exec probes)
  provision/     agent deploy-bundle builder (+ embedded setup.sh / unit)
  reconciler/    reconcile loop, push tracking, status aggregation; SQLite stores
                 (workloads, subnets, services, ingresses)
  registry/      node registry + liveness (heartbeat tracking)
  runtime/       containerd client, CNI bridge, firewall, routes, service LB, NFS volumes
  scheduler/     resource-aware bin-packing placement + assignment tracking
  tls/           ServerConfig / ClientConfig helpers (mTLS, TLS 1.3)
  types/         shared types (Workload, Service, Ingress, Node, Assignment, …)
  ui/            embedded web dashboard (go:embed)
scripts/         setup-server.sh + unit (provisioning); update.sh (rolling update); provision-db.sh (shared Postgres)
docs/specs/      design specs, one per feature
```

---

## Development

```bash
go build ./...        # compile everything
go vet ./...          # static checks
go build -o bin/smith-server ./cmd/server
go build -o bin/smith-agent  ./cmd/agent
```

Generated certificates live under `certs/` (git-ignored) when you run `gencerts`
with a local `-out`. The `bin/` directory is also git-ignored.
