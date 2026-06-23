# Spec: Bin-packing (resource-aware) scheduler

Status: draft for review
Roadmap item: 7

## Problem

The scheduler places replicas by **container count** only: fewest siblings of
the same workload, tie-broken by fewest total assignments. It ignores the
resources a workload requests (`Resources.CPUMillicores`, `MemoryMB`) and the
node's capacity (`Node.CPU`, `Node.MemoryMB`). So it will happily put ten
memory-hungry replicas on a tiny node and OOM them, or schedule onto a node
that's already full while another sits idle.

## Goal

Make placement **resource-aware**: a replica only lands on a node that has
enough free CPU and memory for its request, and among the nodes that fit, pack
according to a chosen strategy. If no node fits, the replica is left pending
(retried next reconcile) rather than overcommitting.

## Design

### 1. Scheduler tracks per-replica requests and per-node usage

`Assign` gains the replica's resource request:

```go
func (s *Scheduler) Assign(replicaID, parentID string, req types.Resources) (types.Assignment, error)
```

The scheduler keeps a third map:

```go
requests map[string]types.Resources // replicaID -> its request
```

Per-node allocated resources = the sum of `requests` for the replicas
currently assigned to that node. (Derived on demand from `assignments` +
`requests`, so no extra bookkeeping on unassign beyond deleting the entry.)

`Unassign` and `ReassignNode` also delete from `requests`.

### 2. Capacity and request units

- Node CPU capacity (millicores) = `Node.CPU * 1000`; memory (MB) =
  `Node.MemoryMB`. **Schedulable capacity = 85% of total** (a 15% reservation,
  `schedulableReserveFraction = 0.15`, held back for the OS and containerd).
- Request = the workload's `Resources` limits as-is. smith has no separate
  request field, so the **limit doubles as the request** (documented
  simplification). A `0` field means "no request" — consumes nothing and fits
  anywhere, preserving today's behavior for workloads without `Resources`.

### 3. Placement algorithm

For an unassigned replica with request `req`:

1. **Filter to fitting nodes**: alive nodes where
   `allocated.CPU + req.CPUMillicores <= capacity.CPU` **and**
   `allocated.Mem + req.MemoryMB <= capacity.Mem`.
2. If none fit → return an "insufficient capacity" error (replica stays
   pending; reconciler logs and retries next tick, like the existing
   "no alive nodes" path).
3. Among fitting nodes, keep the existing **anti-affinity** as the primary
   key: fewest same-parent siblings.
4. Break ties by the **bin-pack score** (the chosen fit strategy), replacing
   today's "fewest total assignments" tiebreak.

Sticky placement is unchanged: an already-assigned replica returns its
existing node (and its recorded request).

### 4. Fit strategy (the bin-packing knob)

Among fitting, equal-sibling nodes, pick by one of:

- **Best-fit (pack)**: the node with the *least* remaining capacity that still
  fits — consolidates replicas onto fewer nodes, leaving large contiguous free
  capacity for big workloads, and lets idle nodes scale down. The classic
  "bin-packing" behavior.
- **Worst-fit (spread)**: the node with the *most* remaining capacity —
  balances utilization, avoids hotspots. Closest to today's count-based
  spread.

Remaining capacity for scoring is a CPU+memory blend (e.g. compare on the more
constrained dimension, or normalized sum). v1 can score on a simple normalized
sum of (free CPU fraction + free memory fraction).

## Reconciler change

One call site: pass the request through.

```go
assignment, err := r.scheduler.Assign(inst.wl.ID, inst.parent, requestOf(inst.wl))
```

where `requestOf` returns `*wl.Resources` or the zero `Resources` when nil.
An "insufficient capacity" error is logged and the replica is skipped this
tick (already how `Assign` errors are handled).

## Testing checklist

1. Two nodes, each "fits" 2 of a 1-replica-per-node-by-memory workload; a 3rd
   replica with a big memory request that doesn't fit anywhere stays pending
   (logged), the other replicas run.
2. Workloads with no `Resources` schedule exactly as before (count-based feel),
   since 0 requests fit anywhere.
3. Best-fit: replicas consolidate onto one node until it's near full before
   using the second. Worst-fit: replicas alternate to balance.
4. Anti-affinity still holds: two replicas of one workload prefer distinct
   nodes even when one node has more room.
5. Freeing a workload returns its capacity (a previously-pending replica then
   schedules).

## Decisions (locked 2026-06-23)

1. **Fit strategy**: **best-fit** — among fitting, equal-sibling nodes, pick
   the one with the least remaining capacity that still fits (pack tight).
2. **Capacity basis**: **reserve 15%** — schedulable = 85% of total node
   CPU/memory (`schedulableReserveFraction = 0.15`).
3. **Priority**: **anti-affinity primary**, bin-pack (best-fit) as the
   tiebreak — replica spread for fault tolerance wins over density.
4. **Request source**: reuse the workload's `Resources` limits as the
   scheduling request; no schema change. A `0` field = no request.
