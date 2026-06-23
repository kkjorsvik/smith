# Spec: Web UI (read-only dashboard)

Status: draft for review
Roadmap item: 8

## Goal

A single-screen cluster dashboard so you can see the whole cluster at a glance
— nodes, what's running where with replica IPs, services and their endpoints,
rollout/health state, and logs — instead of `curl | jq`. Read-only in v1;
write actions (create/delete/scale/rollout) are a documented follow-on.

## Decisions (locked 2026-06-23)

1. **Stack**: one self-contained `index.html` (inline CSS + vanilla JS, no
   build step) embedded in `smith-server` via `go:embed` and served on the
   existing public HTTPS port. Ships with the binary; nothing extra to deploy.
2. **Scope**: read-only dashboard first.
3. **Auth**: the page prompts for the bearer token once and keeps it in
   `localStorage`, sending `Authorization: Bearer <token>` on every API
   `fetch`. On a 401 it clears the token and re-prompts. No server auth change.
4. **Live updates**: poll the read APIs every few seconds and re-render.

## Serving

- New `internal/ui/` package: `index.html` + `//go:embed index.html` exposing
  the bytes (or an `http.FileSystem`).
- `api` serves it at `GET /` (and `GET /ui`) on the **public mux, NOT
  auth-wrapped** — the page is just an app shell with no data; every data
  fetch it makes still hits the auth'd API endpoints with the token. This is
  the standard SPA-shell pattern.
- Same origin as the API (both on `:443`), so no CORS. The Let's Encrypt cert
  means a real `https://smith-server-01.kkjorsvik.com/` with no warnings.

## Data sources (all existing, auth'd)

The dashboard is pure presentation over endpoints that already exist:

- `GET /nodes` — registered nodes: id, host IP, CPU, memory, last heartbeat.
- `GET /workloads` — desired state: id, image, replicas, resources, ports, env.
- `GET /status` — observed: `node -> replica -> {status, pid, ip}`.
- `GET /assignments` — replica -> node, parent.
- `GET /services` — services with ClusterIP/NodePort.
- `GET /workloads/{replicaID}/logs` — logs for a replica (snapshot; `?follow`
  optional later).

## Views (v1)

1. **Nodes** — id, host IP, CPU cores, memory, alive/dead (derived from last
   heartbeat age), and a count of replicas currently running on the node
   (from `/status`). Optionally a rough allocation bar (sum of running
   replicas' requests vs. capacity).
2. **Workloads** — id, image, desired replicas, resources. Expand a row to
   list its replicas with `running/…`, IP, and node (joining `/status` +
   `/assignments`). Surfaces rollout state implicitly: during a roll some
   replicas show the new image / restarting.
3. **Services** — name, target workload, `ClusterIP:Port`, `NodePort`, and a
   live count of ready endpoints (running replicas of the workload).
4. **Logs** — click a replica to open a panel that fetches
   `/workloads/{replicaID}/logs` and shows the output.

## Polling and rendering

- On load: read token from `localStorage`; if absent, show a token input.
- Every ~4s: `fetch` the read endpoints in parallel, join client-side, and
  re-render the tables. Show a "last updated" timestamp and a clear error
  banner if the control plane is unreachable.
- All fetches send the bearer token; a 401 clears it and re-prompts.

## Non-goals (v1)

- No write actions (create/delete/scale/rollout) — follow-on.
- No server-side sessions/login — token-in-browser only.
- No streaming/SSE — polling only (log `?follow` can come with write-actions).
- No multi-cluster, no historical metrics/graphs.

## Testing checklist

1. Open `https://smith-server-01.kkjorsvik.com/` → token prompt; after entering
   the token, the dashboard loads.
2. Nodes, workloads (with replica IPs), and services match `curl` output.
3. Start/stop/scale a workload via `curl` → the dashboard reflects it within a
   poll interval.
4. Trigger a rolling update → replicas visibly cycle in the workload view.
5. Click a replica → its logs render.
6. Wrong/expired token → 401 → re-prompt.

## Build-order note

One commit is reasonable (an HTML file + a small `internal/ui` package + two
routes). Could split shell+nodes/workloads first, then services+logs, but the
surface is small enough to land together.
