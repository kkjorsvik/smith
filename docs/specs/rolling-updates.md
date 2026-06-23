# Spec: Rolling updates

Status: draft for review
Roadmap item: 5 (depends on items 3 & 4)

## Problem

Two gaps:

1. **Spec changes are ignored.** The reconciler's push gate is
   `alreadyPushed := exists && rec.nodeID == assignment.NodeID` — it only
   checks *where* a replica was pushed, not *what*. So re-POSTing a workload
   with a new image (the store upserts it) never updates the running
   containers. The only way to change an image today is delete + recreate.
2. **No gradual rollout.** Even if we re-pushed on change, replacing all
   replicas at once is a full outage.

## Goal

Changing a workload's spec (e.g. a new image) rolls its replicas to the new
spec **a few at a time**, so the service keeps serving throughout. The trigger
is just updating the workload — no new API. The service load balancer (item 4)
already drains a stopping replica and picks up a new one automatically (we
watched the endpoint set follow replica churn), so rollout = controlled replica
replacement.

## Design

### 1. Detect staleness with a spec hash

Add a per-replica spec hash to the push record:

```go
type pushRecord struct {
    nodeID   string
    pushedAt time.Time
    specHash string
}
```

`specHash(wl)` is a sha256 over the fields that define the container —
**Image, Args, Env, Ports, Resources** — and deliberately **excludes Replicas
and MaxUnavailable** (scaling and rollout pacing are not container changes).

A running replica is **stale** when `pushed[replicaID].specHash` differs from
the desired replica's hash.

### 2. Replace in place, rate-limited by MaxUnavailable

Add to `Workload`:

```go
MaxUnavailable int `json:"max_unavailable,omitempty"` // 0/omitted = 1
```

Replacement of a stale replica is recreate-in-place on the same replica ID:
`pushUnassign(node, id)` (agent `StopContainer`, synchronous) → then
`pushAssignment(node, newSpec)` (agent runs the new container). Because
`StopContainer` is synchronous on the agent, the old container is fully gone
before the new one is created (no ID collision).

The rate limiter bounds how many replicas of a workload are **not Running** at
once:

```
unavailable[parent] = count of the workload's desired replicas whose status is not Running
for each stale, currently-Running replica (in stable -0,-1,-2 order):
    if unavailable[parent] < maxUnavailable:
        replace it (unassign -> assign), set pushed.specHash = desired
        unavailable[parent]++   // it's now down, counts against the budget
```

Because a replica that is down for *any* reason (crash, initial pull) counts
toward `unavailable`, a rollout won't take down a healthy replica while others
are already unavailable. As each replaced replica comes back Running on the new
spec, `unavailable` drops and the budget frees the next one. With
`maxUnavailable: 1` and 3 replicas, the roll is sequential: at most one replica
down at a time, ~1/3 capacity reduction.

### 3. Reconcile loop integration

The assign/push loop becomes three cases per desired replica:

```
desiredHash := specHash(inst.wl)
rec, exists := pushed[replicaID]
switch {
case !exists || rec.nodeID != node:        // initial placement / moved node
    pushAssignment; pushed[replicaID] = {node, now, desiredHash}
case rec.specHash != desiredHash:           // stale — roll, rate-limited
    if unavailable[parent] < maxUnavailable {
        pushUnassign(node, replicaID); pushAssignment(node, inst.wl)
        pushed[replicaID] = {node, now, desiredHash}
        unavailable[parent]++
    }
default:                                     // up to date — nothing to do
}
```

Compute `unavailable[parent]` once per reconcile from a single cluster status
snapshot (reuse the status the loop already fetches for the running-check,
rather than adding fan-out). The existing "not running → repush" path keeps
crash recovery working and now repushes the *desired* (possibly new) spec, so a
crashed replica naturally returns on the new image.

### 4. Readiness signal

A replica is "ready" when `AggregateStatus` reports it `Running`. Health-check
readiness (the `HealthCheck` type exists but isn't wired) is a future
refinement; v1 gates on Running, which is enough to pace the roll.

### 5. Trigger and the service during a roll

- **Trigger**: re-`POST /workloads` with the changed spec. The store upserts;
  the reconciler sees the hash mismatch and rolls. No rollout API.
- **Service**: a replica being replaced is briefly not Running, so item 4's
  endpoint computation drops it within a tick; the new one rejoins when
  Running. With `maxUnavailable: 1`, the service never loses more than one
  backend at a time.

## Testing checklist

1. Deploy `smith-nginx` replicas:3 + service `web`. Re-POST with a different
   image (e.g. `nginx:1.27-alpine`). Watch `/status`: replicas flip to the new
   image one at a time, never more than one missing at once.
2. `curl` the ClusterIP throughout the roll — it keeps answering.
3. Set `max_unavailable: 2` → two replicas roll concurrently.
4. Re-POST with no change → no churn (hashes match).
5. Change only `replicas` → scale up/down, NOT a roll (hash unchanged).
6. A mid-roll replica crash still counts against the budget (no extra
   simultaneous downtime).

## Decisions (locked 2026-06-23)

1. **Strategy**: recreate-in-place, rate-limited by `MaxUnavailable`. Replace
   stale replicas on the same IDs, no surge replicas.
2. **Default `MaxUnavailable`**: `1` — sequential, one replica down at a time.
3. **Readiness signal**: container `Running` (from `AggregateStatus`). Wiring
   the `HealthCheck` probes into rollout readiness is a future refinement.
4. **Trigger**: implicit — re-`POST /workloads` with a changed spec rolls it.
   No rollout API.
