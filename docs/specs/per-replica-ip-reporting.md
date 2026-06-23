# Spec: Per-replica container IP reporting

Status: draft for review
Roadmap item: 2 (depends on item 1 cross-node networking; foundation for
multi-replica workloads and the service load balancer)

## Goal

Surface each container's CNI-assigned IP through the existing status pipeline,
so the control plane (and ultimately a service load balancer) can see where
every replica is reachable. Now that container IPs are cluster-routable (item
1), the IP is a useful cluster-wide address rather than a node-local one.

## Where the IP already exists

`CNI.Setup` (internal/runtime/cni.go) already returns the assigned IP, but
`RunContainer` discards it:

```go
if _, err := opts.CNI.Setup(ctx, opts.ID, netnsPath, opts.Ports); err != nil {
```

The IP is only known at Setup time — containerd itself doesn't expose it — so
it must be captured there and associated with the container ID for the status
path (`ListRunning`) to report it.

## Design

Keep the IP in the runtime layer where Setup/teardown happen, so the rest of
the pipeline carries it for free.

### 1. ContainerStatus gains an IP field (internal/runtime/containerd.go)

```go
type ContainerStatus struct {
	ID     string                   `json:"id"`
	Status containerd.ProcessStatus `json:"status"`
	Pid    uint32                   `json:"pid"`
	IP     string                   `json:"ip,omitempty"`
}
```

`omitempty` keeps the field absent for containers with no CNI IP (CNI
disabled, or status reported before Setup).

### 2. Client owns an IP map keyed by container ID

```go
type Client struct {
	inner *containerd.Client
	mu    sync.Mutex
	ips   map[string]string // containerID -> CNI IP
}
```

Initialize `ips` in `NewClient`. Helpers:

```go
func (c *Client) setIP(id, ip string)  // store under mu
func (c *Client) clearIP(id string)    // delete under mu
func (c *Client) getIP(id string) string
```

### 3. Capture the IP at Setup, clear it at teardown

- `RunContainer`: capture the Setup return and record it:
  ```go
  ip, err := opts.CNI.Setup(ctx, opts.ID, netnsPath, opts.Ports)
  if err != nil { ... }
  c.setIP(opts.ID, ip)
  ```
- Clear the IP wherever the container is removed, alongside the existing CNI
  teardown / container delete: `RunContainer`'s normal-exit path and its
  error-path deferred cleanup, `StopContainer`, and `Cleanup`. A stale entry
  is harmless (ListRunning only reports IPs for containers that still exist),
  but clearing keeps the map bounded across many run/stop cycles.

### 4. ListRunning merges the IP

```go
status := ContainerStatus{ID: container.ID(), Status: containerd.Unknown}
status.IP = c.getIP(container.ID())
// ... existing task status / pid lookup ...
```

### 5. Nothing else changes

The IP rides the existing pipeline unchanged:
- agent `handleStatus` already returns `c.ListRunning()` verbatim → now
  includes `ip`.
- reconciler `fetchAgentStatus` / `AggregateStatus` decode `ContainerStatus`
  → `ip` is carried.
- control-plane `GET /status` returns nodeID → containerID → ContainerStatus
  → operators now see `"ip": "10.22.1.7"` per replica.

## Behavior notes

- **In-memory, rebuilt on restart**: the IP map is not persisted. On agent
  restart, `CleanupAll` removes all containers and the reconciler re-pushes
  them, so Setup repopulates IPs. Consistent with the existing wipe-and-
  rebuild-on-restart model; no persistence needed.
- **Reporting window**: a container appears in `ListRunning` between task
  creation and CNI Setup with an empty `ip` for a sub-second window; it fills
  in once Setup completes. `omitempty` means consumers must treat a missing IP
  as "not yet known," not "no IP."
- **CNI-disabled nodes**: `ip` is simply absent.

## Testing checklist

1. Start a workload with CNI on a node; `GET /status` shows its `ip` within
   the container's subnet (e.g. `10.22.1.x`).
2. The reported IP matches `crictl`/`ip addr` inside the container's netns.
3. Stop the workload; the IP no longer appears in `/status`.
4. Run two workloads on the same node; each reports a distinct IP.
5. Restart the agent; after re-push, IPs reappear.

## Decisions (locked 2026-06-23)

1. **Scope**: report IP only via the existing `/status` aggregation. No
   dedicated control-plane `workloadID -> []IP` index yet — the service load
   balancer (item 4) will consume `AggregateStatus` when built, so an index
   with no consumer is deferred.
2. **IP map ownership**: `runtime.Client` holds the IP map. Setup/teardown
   already live there, so `ListRunning` merges it and the agent/reconciler/API
   layers need no changes.
