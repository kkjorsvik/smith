# Spec: Stateful workloads (NFS-backed volumes)

Status: draft for review
Roadmap item: 10 (new — needed to host real stateful services: Postgres, etc.)

## Problem

smith is hostile to state today:

1. **Containers are ephemeral.** The writable layer is discarded on every
   recreate — and smith recreates constantly (rolling updates recreate in place;
   `CleanupAll` wipes the namespace on agent restart). A Postgres data dir in the
   container layer would vanish.
2. **Replicas move.** The scheduler spreads by anti-affinity and reschedules off
   dead nodes — a DB can't follow its data to another machine.
3. **No volume concept.** `Workload` has no notion of a persistent mount.

Goal: run single-instance stateful services (Postgres for forgejo/n8n, etc.)
with data that survives container recreation, rolling updates, agent restarts,
and node failover.

## Approach: shared NFS storage

Data lives on an always-on **NFS share** (an Unraid export), reachable from every
node. Because any node can mount the data, a stateful replica can **fail over**
like a stateless one — the existing sticky-schedule + reschedule-on-death logic
already does the right thing; the rescheduled replica mounts the same NFS dir and
resumes. The only new constraint is **single-writer**: a volume-bearing workload
is clamped to `replicas: 1`.

```
workload (replicas=1, volume) --scheduled--> node X
                                               |  agent mounts NFS subdir,
                                               |  bind-mounts into container
                                               v
                            unraid:/mnt/user/smith/<workloadID>/<vol>  (NFS)
node X dies -> reschedule to node Y -> Y mounts the same subdir -> Postgres resumes
```

## Part 1 — Types

```go
// Volume is a persistent mount backed by the cluster NFS share.
type Volume struct {
    Name string `json:"name"` // unique within the workload; the NFS subdir name
    Path string `json:"path"` // mount path inside the container, e.g. /var/lib/postgresql/data
}

// Workload gains:
Volumes []Volume `json:"volumes,omitempty"`
```

A workload with `volumes` is **stateful**. Validation on `POST /workloads`:
clamp/require `replicas <= 1` (reject >1 with a clear error — multi-writer on one
data dir corrupts). Volume `name` is `[a-z0-9-]+`; `path` must be absolute.

The container subpath is derived, not specified: `<workloadID>/<name>`. One smith
share, auto-subdivided per workload — no per-workload server config.

## Part 2 — Cluster NFS share config

The share is configured **per agent** via `agent.env`:

```
SMITH_NFS_SOURCE=unraid.kkjorsvik.com:/mnt/user/smith
```

`add-agent` gains an optional `-nfs` flag that writes this into the bundle's
`agent.env`; `setup-agent.sh` installs `nfs-common`. An agent with no
`SMITH_NFS_SOURCE` simply can't run volume-bearing workloads (it logs and the
assign fails), so non-storage clusters are unaffected.

> Alternative considered: each volume carries its own `nfs: "host:/export"`
> string (self-contained, no cluster config, but verbose and hardcodes the
> server in every workload). Rejected for v1 in favor of the single-share model,
> which matches "one share for smith."

## Part 3 — Agent: mount + bind into the container

On agent startup, if `SMITH_NFS_SOURCE` is set, mount it **once** at
`/var/lib/smith/nfs` (idempotent; `nfs-common` provides `mount.nfs`). Per-volume
work happens at container assign:

1. `mkdir -p /var/lib/smith/nfs/<workloadID>/<name>` (first run creates it on the
   share).
2. Add an OCI bind mount (`oci.WithMounts`) from that host dir to the volume's
   container `path`. The spec is already built from option slices, so this is an
   added `specOpts` entry.

Teardown just removes the container; the NFS mount stays for the agent's life and
the data stays on the share. `CleanupAll` only deletes containerd
containers/snapshots — it never touches the host/NFS dirs, so data is safe across
recreation and agent restarts.

## Part 4 — Scheduling & failover (mostly unchanged)

No scheduler changes needed: stateful workloads use the existing sticky
placement, and reschedule-on-death is now *desirable* (NFS data is reachable on
the new node). The reconciler must only ensure a stateful workload is never
expanded past one replica.

> Split-brain caveat: if a node is network-partitioned but still running, smith
> reschedules the replica after the 30s heartbeat timeout while the old one may
> still be writing — two writers on one NFS dir. Postgres's `postmaster.pid`
> guard is unreliable over NFS. Proper fencing (STONITH) is out of scope; on a
> trusted LAN this risk is low and accepted for v1. Backups remain the real
> durability net.

## Part 5 — Connectivity

A stateful workload is fronted by a **service** (ClusterIP) so apps reach it by a
stable address; no ingress (Postgres isn't HTTP). Nothing new — services already
work.

## Sub-commits

1. **Types + validation**: `Volume`, `Workload.Volumes`, replicas<=1 clamp,
   spec-hash includes volumes. Inert to agents.
2. **Agent mount + bind**: NFS mount-at-startup, per-volume mkdir + OCI bind
   mount, `SMITH_NFS_SOURCE` wiring.
3. **Provisioning**: `add-agent -nfs`, `nfs-common` in `setup-agent.sh`, docs.

## Testing checklist

1. Postgres workload with a volume at `/var/lib/postgresql/data`, `replicas:1`,
   + a `postgres` service (ClusterIP). Write a row.
2. Delete the workload's container / trigger a rolling update (new image) → data
   persists (same row present).
3. Restart the agent → container recreated, data intact.
4. Kill the node → replica reschedules to another node, mounts the same NFS dir,
   Postgres comes up with the row.
5. `POST /workloads` with `volumes` and `replicas:3` → rejected.
6. Remove the workload → NFS data dir is left in place (not deleted).

## Decisions (locked 2026-06-23)

1. **Storage**: NFS / shared-backed (always-on Unraid export). Enables failover
   for stateful workloads since any node can mount the data.
2. **Multi-replica**: enforce `replicas <= 1` for volume-bearing workloads
   (single-writer). Per-replica volumes / StatefulSet identity deferred.
3. **Share model**: one cluster smith share, auto-subdivided as
   `<workloadID>/<name>`, configured per agent via `SMITH_NFS_SOURCE`.
4. **Data deletion**: volume data is never auto-deleted on workload removal.

## Out of scope / future

- Local-disk volumes (no shared storage) and other backends (Ceph, iSCSI).
- Per-replica volumes / StatefulSet-style stable identity for replicated apps.
- Fencing / split-brain protection.
- smith-managed backups/snapshots of volume data.
- Dynamic volume provisioning / quotas.
