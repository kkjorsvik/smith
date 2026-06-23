# Spec: Service + load balancer

Status: draft for review
Roadmap item: 4 (depends on items 1–3; consumes replica IPs)

## Goal

Give a workload a stable virtual endpoint — a **ClusterIP** (VIP) on a
service port — that load-balances across the workload's running replica IPs.
A client connects to `<clusterIP>:<port>` from anywhere in the cluster and
lands on one of the replicas, regardless of which node it's on. This is the
payoff of items 1–3: replica IPs are cluster-routable (1), reported (2), and
plural (3).

Model: kube-proxy's iptables mode, scaled down. Each agent programs nat-table
DNAT rules that catch traffic to the VIP and randomly DNAT it to a replica IP;
connection tracking pins each connection to its chosen backend.

## Architecture (mirrors cross-node routing)

```
Control plane                                  Agent (each node)
-------------                                  -----------------
ServiceIPAllocator (SQLite) — VIP per service  service-sync loop (ticker):
service store (CRUD)                             GET /nodes/{id}/services
endpoint computation:                            program nat rules:
  assignments(parent→replica) ∩ status(IP,run)     SMITH-SERVICES (PREROUTING+OUTPUT)
GET /nodes/{id}/services → [{vip,port,proto,        SMITH-SVC-<id> per service
  targetPort, endpoints[]}]                         random DNAT to each endpoint
```

## Part 1 — Types (internal/types)

```go
// Service is a stable virtual endpoint load-balancing across a workload's
// replica IPs. Reachable internally at ClusterIP:Port and externally at
// <anyNodeIP>:NodePort.
type Service struct {
    Name       string `json:"name"`        // unique service ID
    WorkloadID string `json:"workload_id"` // selector: replicas of this workload
    Port       int    `json:"port"`        // service (VIP) port
    TargetPort int    `json:"target_port"` // container port to forward to
    Protocol   string `json:"protocol,omitempty"`     // tcp (default) / udp
    ClusterIP  string `json:"cluster_ip,omitempty"`   // assigned VIP (read-only)
    NodePort   int    `json:"node_port,omitempty"`    // assigned host port on every node (read-only)
}

// ServiceEndpoints is what the agent needs to program rules: the service plus
// the current set of backend replica IPs.
type ServiceEndpoints struct {
    ClusterIP  string   `json:"cluster_ip"`
    Port       int      `json:"port"`
    NodePort   int      `json:"node_port"`
    TargetPort int      `json:"target_port"`
    Protocol   string   `json:"protocol"`
    Endpoints  []string `json:"endpoints"` // replica IPs, running only
}
```

## Part 2 — ClusterIP allocation (control plane, persisted)

A new VIP pool, **distinct from the pod CIDR**: default `10.23.0.0/16`
(`ServiceCIDR`). Allocate one IP per service (sequential lowest-free), persisted
in SQLite keyed by service name — same persistence discipline as
`SubnetAllocator` (idempotent, survives restart, freed only on service delete).

```go
type ServiceIPAllocator struct { ... } // state.db, table service_ips(name PK, ip UNIQUE)
func (a *ServiceIPAllocator) Allocate(name string) (string, error) // idempotent
func (a *ServiceIPAllocator) Release(name string) error
```

**NodePort allocation**: each service also gets a host port from the
`30000–32767` range (kube-style), allocated lowest-free and persisted keyed by
service name (same allocator pattern, distinct table/column). The same NodePort
is opened on every node. The two allocations can live in one `services` row
(`cluster_ip`, `node_port`) rather than separate tables — a single
`ServiceAllocator` returning `(clusterIP, nodePort)` is fine.

## Part 3 — Service store + API (control plane)

Persist services in a `services` table (name, workload_id, port, target_port,
protocol, cluster_ip). Public, auth'd endpoints:

- `POST /services` — create/update; allocates a ClusterIP, returns the Service
  with `cluster_ip` filled in.
- `GET /services` — list.
- `DELETE /services/{name}` — remove + `Release` the VIP.

## Part 4 — Endpoint computation (control plane)

For a service selecting workload `W`, the backend set is the IPs of `W`'s
running replicas. Compute from data we already have (no new tracking):

```go
// assignments: replicaID -> (nodeID, parentID)   [scheduler.ListAssignments]
// status:      nodeID -> replicaID -> {IP, Status} [reconciler.AggregateStatus]
for _, a := range scheduler.ListAssignments() {
    if a.ParentID != svc.WorkloadID { continue }
    s := status[a.NodeID][a.WorkloadID]
    if s.Status == running && s.IP != "" {
        endpoints = append(endpoints, s.IP)
    }
}
```

Only running replicas with a known IP become endpoints, so rolling restarts
and failures drop out automatically.

## Part 5 — Distribution (control plane → agent)

`GET /nodes/{id}/services` (internal mTLS) returns `[]ServiceEndpoints` for all
services, with endpoints computed as above. Agents pull on a ticker (heartbeat
cadence), exactly like routes. The endpoint set is identical on every node, so
any node can balance to any replica (replica IPs are cluster-routable).

## Part 6 — Agent service proxy (iptables)

New `runtime.ServiceProxy` (go-iptables), reconciling nat rules to match the
pulled `[]ServiceEndpoints`. Structure (kube-proxy iptables mode, simplified):

- One parent chain `SMITH-SERVICES` in nat, jumped once from `PREROUTING` and
  `OUTPUT` (so both pod-originated and host-originated traffic is caught).
- Per service, a chain `SMITH-SVC-<hash(name)>`:
  - ClusterIP rule (in `SMITH-SERVICES`):
    `-d <clusterIP>/32 -p <proto> --dport <port> -j SMITH-SVC-<h>`.
  - NodePort rule (in `SMITH-SERVICES`, after the ClusterIP rules — a
    `SMITH-NODEPORTS` sub-section): `-p <proto> -m addrtype --dst-type LOCAL
    --dport <nodePort> -j SMITH-SVC-<h>`. The `addrtype LOCAL` match catches
    traffic to any local node IP on the NodePort.
  - Inside `SMITH-SVC-<h>`, N endpoint rules using the statistic module for
    uniform random selection, each DNAT'ing to `<endpointIP>:<targetPort>`:
    - rule i (i=0..N-2): `-m statistic --mode random --probability 1/(N-i) -j DNAT --to-destination epIP:targetPort`
    - rule N-1: `-j DNAT --to-destination epIP:targetPort` (probability 1)
  - Connection tracking (DNAT is conntracked) pins each connection to its
    chosen backend, giving per-connection (not per-packet) balancing.
- Open the NodePort on the host: `Firewall.OpenPort(nodePort, proto)` (reuses
  the existing INPUT-accept helper) so external clients can reach it; close it
  on service delete.
- Reconcile each tick: rebuild each service's chain to match its current
  endpoints (ClearChain + repopulate is simplest and idempotent), add the
  parent jumps if missing, and delete chains + close NodePorts for services
  that no longer exist.
- A `0` endpoints service: chain exists but has no DNAT rule, so the VIP /
  NodePort is unreachable until a replica is ready.

go-iptables provides `NewChain`/`ClearChain`/`DeleteChain`/`ListChains`/
`AppendUnique`, which cover this.

## Part 7 — SNAT / masquerade (hairpin + NodePort)

A masquerade rule is required in two cases, so it's included:
- **Hairpin**: a replica connecting to its own service and being balanced back
  to itself — without SNAT the source==destination reply bypasses conntrack and
  hangs.
- **NodePort from an external client**: client → `node:nodePort` → DNAT to a
  replica (possibly on another node); the replica must reply to the *ingress
  node*, not the external client, or the asymmetric path breaks. SNAT to the
  node makes the reply return through it.

Implementation (kube-proxy's mark approach): in `SMITH-SVC-<h>`, mark packets
destined for an endpoint that need masquerading, and add one
`-j MASQUERADE` rule in `POSTROUTING` matching that mark. Simplest workable
form for v1: masquerade traffic entering a service chain whose source is not in
the pod CIDR, plus the hairpin self-case. Reuses the nat table the firewall
already manages.

## Part 8 — Build order (sub-commits, like cross-node networking)

1. **Allocation + store + API**: types, `ServiceIPAllocator`, `services` table,
   `POST/GET/DELETE /services`. Inert to agents. Testable: create a service,
   see a ClusterIP assigned and persisted across restart.
2. **Endpoint computation + distribution**: `GET /nodes/{id}/services`.
   Testable: curl it, see the right replica IPs.
3. **Agent service proxy**: `ServiceProxy` + service-sync loop programming
   iptables. Testable: `curl <clusterIP>:<port>` from a node/container hits
   replicas; killing a replica drops it from rotation within a tick.

## Testing checklist

1. Create service `web` → workload `smith-nginx` (3 replicas), port 80 →
   target 80; get a ClusterIP (e.g. `10.23.0.1`).
2. From a container and from a node, `curl 10.23.0.1:80` repeatedly → responses
   served by different replicas (verify via per-replica content or logs).
3. Scale replicas 3→1 → service still answers, balanced to the survivor within
   a tick.
4. Kill a node → its replicas drop from endpoints; service keeps answering.
5. Restart control plane → same ClusterIP (persistence).
6. Delete service → VIP released, iptables chains removed on every node.

## Decisions (locked 2026-06-23)

1. **Endpoint type**: ClusterIP **and** NodePort. Internal stable VIP
   (`ClusterIP:Port`) plus an external host port (`<anyNodeIP>:NodePort`) on
   every node, from the `30000–32767` range.
2. **Balancing**: iptables statistic-random + conntrack (per-connection). No
   new deps; reuses go-iptables. IPVS not used.
3. **Service CIDR**: `10.23.0.0/16` for VIPs, distinct from the pod
   `10.22.0.0/16`.
4. **Selector**: by `workloadID` — a service targets exactly one workload's
   replicas.
5. **DNS**: none in v1 — services reached by ClusterIP or NodePort. A name→VIP
   resolver is a possible follow-on.
6. **Masquerade**: include the SNAT/masquerade-mark rule — required for both
   hairpin-to-self and NodePort-from-external correctness.
