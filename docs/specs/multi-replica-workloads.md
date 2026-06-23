# Spec: Multi-replica workloads

Status: draft for review
Roadmap item: 3 (depends on items 1 & 2; sets up the service + load balancer)

## Goal

Let a workload declare `replicas: N` and have smith run N instances spread
across nodes. Each replica gets its own cluster-routable IP (item 1) reported
through `/status` (item 2). This is the backend pool the service + load
balancer (item 4) will front.

## Core idea: expand a workload into N replica instances

The whole single-container pipeline — scheduler, reconciler push/status,
agent container lifecycle, `/status` aggregation — already works per container
ID. So model a replica as a derived workload with a unique ID, and **expand**
an N-replica workload into N instances at the reconciler. Almost everything
downstream is unchanged; the new logic is the expansion and the scheduler's
spread.

Replica ID: `<workloadID>-<index>`, e.g. `smith-nginx-0`, `smith-nginx-1`.
The replica's container ID, `/status` key, and scheduler assignment key are
all this replica ID.

## Part 1 — Workload schema (internal/types + store)

Add to `Workload`:

```go
Replicas int `json:"replicas,omitempty"`
```

`0` or omitted means 1 (default). Persist via the incremental migration
framework: `replicas INTEGER` column, COALESCE-guarded read defaulting to 1
when NULL/0.

## Part 2 — Reconciler expansion (internal/reconciler)

`reconcile()` currently iterates `store.List()` (workloadID -> Workload)
directly. Insert an expansion step that turns the desired workloads into a
desired **replica set**:

```go
// replicaID -> the per-replica workload (ID set to the replica ID) + parent.
type replica struct {
    wl     types.Workload // ID = "<parent>-<i>", same image/args/env/ports/resources
    parent string
}

func expand(desired map[string]types.Workload) map[string]replica {
    out := make(map[string]replica)
    for id, w := range desired {
        n := w.Replicas
        if n < 1 { n = 1 }
        for i := 0; i < n; i++ {
            rw := w
            rw.ID = fmt.Sprintf("%s-%d", id, i)
            out[rw.ID] = replica{wl: rw, parent: id}
        }
    }
    return out
}
```

Drive the existing loop over this expanded map instead of raw workloads:
- assign each replica (`scheduler.Assign(replicaID, parentID)`),
- push the replica's workload to its node (container ID = replica ID),
- track `pushed` by replica ID, status-check by replica ID,
- the "stop containers no longer desired" loop compares assignments against
  the expanded desired set, so scaling down (N: 3 -> 1) stops `-1`, `-2`
  automatically.

No change to `pushAssignment`/`pushUnassign`/`fetchAgentStatus` — they already
take a node + a workload/ID.

## Part 3 — Scheduler: replica-aware placement with spread (internal/scheduler)

Change `Assign` to be replica-aware and spread replicas of the same parent
across distinct nodes (anti-affinity), falling back to least-loaded.

```go
type Scheduler struct {
    registry    *registry.Registry
    mu          sync.RWMutex
    assignments map[string]string // replicaID -> nodeID
    parents     map[string]string // replicaID -> parentID (for anti-affinity)
}

func (s *Scheduler) Assign(replicaID, parentID string) (types.Assignment, error)
```

Placement when `replicaID` is not already assigned (sticky otherwise):
1. For each alive node, compute `(siblings, load)` where `siblings` = number
   of already-assigned replicas with the same `parentID` on that node, and
   `load` = total assignments on that node.
2. Pick the node minimizing `siblings` first, then `load`. This places each
   new replica on a node with the fewest siblings (spreads one-per-node while
   nodes are available), breaking ties by least total load.

`Unassign` and `ReassignNode` also delete from `parents`. `ListAssignments`
may include `ParentID` on `types.Assignment` for observability (optional).

Dead-node failover composes for free: an evicted replica is rescheduled and
the spread logic prefers a node not already running a sibling.

## Part 4 — Agent

No changes. The agent receives a workload whose ID is the replica ID and runs
a container under that ID, exactly as today. `/status` keys by container ID =
replica ID, so per-replica status (with IPs) already flows.

## Naming of the single-replica case

For uniformity and to make item 4's "find replicas of workload X" trivial
(`X-*`), **always** suffix — a 1-replica `smith-nginx` runs as
`smith-nginx-0`. This renames any currently-running bare-ID workload once on
the next reconcile (old container stopped, `-0` started). Acceptable on a dev
cluster; called out as the only migration cost.

## Host ports + replicas (known limitation)

A replica carries the parent's `Ports`. Host port mappings only compose with
replicas while the scheduler keeps **≤1 replica per node** (the spread does
this until `replicas > nodes`). If replicas stack on a node (more replicas
than nodes), the second replica's host-port mapping collides (CNI portmap /
firewall) and its setup fails, though its container IP still works. This is
expected: multi-replica access is the **service load balancer's** job (item
4), which balances across replica IPs — host ports are a single-instance
convenience. The spec does not try to make host ports work for stacked
replicas.

## Scale up / down

- Scale up (N: 1 -> 3): expansion yields `-0..-2`; `-1`,`-2` are new, get
  assigned (spread) and pushed.
- Scale down (N: 3 -> 1): expanded desired set is just `-0`; `-1`,`-2` fall
  into the existing "no longer desired" path → unassigned and stopped.
- Delete workload: all replicas stop (as today, now per replica).

## Testing checklist

1. `replicas: 3` on a 2-node cluster → `/status` shows `-0`,`-1`,`-2` with
   distinct IPs; spread is 2 on one node, 1 on the other (no node has all 3).
2. Each replica reachable by its IP across nodes.
3. Scale to 1 → `-1`,`-2` stop; `-0` remains.
4. Kill a node → its replicas reschedule onto the surviving node.
5. Existing single workload comes back as `-0`.

## Decisions (locked 2026-06-23)

1. **Replica naming**: always suffix — a 1-replica workload runs as
   `smith-nginx-0`. Uniform; makes item 4's `X-*` replica discovery trivial.
   One-time rename of existing single workloads on the next reconcile.
2. **Spread**: prefer-spread-then-stack — one-per-node while nodes are
   available, then stack extras on least-loaded nodes. `replicas > nodes`
   runs all replicas (host ports degrade on stacked nodes; container IPs are
   fine).
3. **Status rollup**: per-replica only via the existing `/status` aggregation.
   No dedicated rollup endpoint — item 4 consumes `AggregateStatus`.
