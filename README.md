# smith

A small, self-contained container orchestrator. A **control plane**
(`smith-server`) keeps a desired set of workloads running across a fleet of
**agent** nodes (`smith-agent`). Each agent owns the container lifecycle locally
via [containerd](https://containerd.io/); the control plane schedules workloads
onto nodes, continuously reconciles desired vs. actual state, fails workloads
over when a node dies, and reports aggregated cluster status.

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
- [Workloads](#workloads)
- [API reference](#api-reference)
- [How it works](#how-it-works)
- [Operational notes & caveats](#operational-notes--caveats)
- [Project layout](#project-layout)
- [Development](#development)

---

## Features

- **Multi-node scheduling** — workloads are placed on the alive node with the
  fewest assignments (least-loaded).
- **Reconciliation loop** — the control plane drives actual state toward desired
  state every 5 seconds: pushing new assignments, stopping removed workloads,
  and re-pushing anything that isn't actually running.
- **Failover** — when a node misses its heartbeat window it is declared dead and
  its workloads are rescheduled onto healthy nodes.
- **Persistent desired state** — workloads are stored in SQLite, so the desired
  set survives a control-plane restart.
- **Cluster-wide status** — `GET /status` fans out to every alive agent and
  returns real per-container state (status + PID) keyed by node.
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
| `smith-server` | Control plane: serves the public + internal APIs, runs the scheduler and reconcile loop, persists desired state. Also provides the `gencerts` subcommand. |
| `smith-agent` | Worker: registers with the control plane, heartbeats, and serves an mTLS API to receive assignments and report container status. Manages containers via the local containerd. |

---

## Ports

| Component | Port | Protocol | Purpose |
|-----------|------|----------|---------|
| smith-server | `:443` | HTTPS (Let's Encrypt) | Public workload API |
| smith-server | `:9443` | mTLS (Smith CA) | Internal API: node register + heartbeat |
| smith-agent | `:9000`\* | mTLS (Smith CA) | Receives assign/unassign/status from the control plane |

\* Whatever host:port you pass to the agent's `-addr` flag.

---

## Prerequisites

- **Go 1.26+** (module targets `go 1.26.4`).
- **containerd** running on every host (control plane and agents), reachable at
  its default socket. The server and agent connect to the local containerd on
  startup and clean up the `smith` namespace.
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
| `/var/lib/smith/state.db` | server | SQLite desired-state store |
| `/var/lib/smith/autocert/server.{crt,key}` | server | Cached public Let's Encrypt cert |
| `smith-server-01.kkjorsvik.com` | server cert + public ACME domain | The public hostname; also the server cert CN |
| `:443`, `:80`-free, `:9443` | server | Public HTTPS / internal mTLS (no `:80` is used — DNS-01 needs no HTTP listener) |
| `5s` reconcile interval | server | How often desired/actual state is reconciled |
| `10s` heartbeat interval | agent | How often an agent pings the control plane |
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

## Workloads

A workload describes a container the cluster should keep running:

```json
{
  "id": "web",
  "image": "docker.io/library/nginx:latest",
  "args": [],
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
| `id` | string | Unique workload ID (also the container ID) |
| `image` | string | Fully-qualified image ref (pulled if not present locally) |
| `args` | string[] | Command arguments |
| `health_check` | object? | Optional probe (see below) |

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

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/workloads` | List desired workloads |
| `POST` | `/workloads` | Add a workload (JSON body as above) |
| `DELETE` | `/workloads/{id}` | Remove a workload (reconciler stops it next tick) |
| `GET` | `/status` | Cluster-wide container status, aggregated from every alive agent |
| `GET` | `/nodes` | Registered nodes and their last heartbeat |
| `GET` | `/assignments` | Current workload → node assignments |

### Internal API — `:9443` (mTLS, used by agents)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/nodes/register` | Agent registration |
| `POST` | `/nodes/{id}/heartbeat` | Agent heartbeat |

### Agent API — agent `-addr` (mTLS, used by the control plane)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/assign` | Start/keep a workload's container |
| `DELETE` | `/assign/{id}` | Stop a workload's container |
| `GET` | `/status` | This node's observed container state |

### Examples

Add a workload (note the bearer token):

```bash
curl https://smith-server-01.kkjorsvik.com/workloads \
  -H "Authorization: Bearer $(sudo cat /etc/smith/token)" \
  -H 'Content-Type: application/json' \
  -d '{"id":"web","image":"docker.io/library/nginx:latest","args":[]}'
```

`GET /status` returns observed state aggregated across agents, keyed by node ID,
then container ID:

```json
{
  "smith-agent-01": {
    "web": { "id": "web", "status": "running", "pid": 12345 }
  },
  "smith-agent-02": {}
}
```

A node with no reported containers (or one that was unreachable this cycle)
appears with an empty object rather than being omitted. Container `status`
values come from containerd: `running`, `created`, `stopped`, `paused`,
`unknown`.

---

## How it works

**Registration & heartbeats.** An agent `POST`s to `/nodes/register` on startup,
then heartbeats every **10s**. The control plane marks a node **dead** if it has
not heartbeated within **30s** (`HeartbeatTimeout`).

**Scheduling.** When a workload needs a node, the scheduler picks the **alive
node with the fewest current assignments**. Assignments are sticky — an already-
assigned workload keeps its node until that node dies or the workload is removed.

**Reconcile loop (every 5s).** Each tick the control plane:

1. **Evicts dead nodes** — workloads on a dead node are unassigned (and their
   push records cleared) so they get rescheduled; the node is removed.
2. **Assigns & pushes** — every desired workload is assigned a node, and the
   assignment is pushed to that agent over mTLS **only when it is new or the node
   changed** (not on every tick).
3. **Verifies running state** — for each pushed workload past a grace period of
   `2 × interval` (**10s**), it fetches the agent's `/status`; if the container
   isn't `running`, the push record is cleared so it is re-pushed next tick. The
   grace period prevents a freshly-started container from being torn down before
   it reaches `running`.
4. **Stops removed workloads** — anything assigned but no longer desired is
   unassigned and the agent is told to stop it.

**Status aggregation.** `GET /status` calls `Reconciler.AggregateStatus`, which
fans out to every alive agent's `/status` over mTLS and merges the results by
node. (The control plane has no local workloads — they all run on agents — so it
must aggregate rather than read its own containerd.)

**Failover, end to end.** Node dies → misses heartbeats → declared dead at 30s →
its workloads unassigned → rescheduled onto the least-loaded surviving node →
pushed there → verified running. Desired state is read from SQLite, so this holds
across a control-plane restart.

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

---

## Project layout

```
cmd/
  server/        smith-server entrypoint (+ gencerts subcommand)
  agent/         smith-agent entrypoint
internal/
  acme/          Route 53 DNS-01 ACME solver
  agent/         agent: register, heartbeat, serve mTLS API, manage containers
  api/           control-plane HTTP APIs (public :443, internal :9443)
  health/        health-check Monitor (http/exec probes)
  reconciler/    reconcile loop, push tracking, status aggregation, SQLite store
  registry/      node registry + liveness (heartbeat tracking)
  runtime/       containerd client wrapper
  scheduler/     least-loaded placement + assignment tracking
  tls/           ServerConfig / ClientConfig helpers (mTLS, TLS 1.3)
  types/         shared types (Workload, Node, Assignment, HealthCheck)
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
