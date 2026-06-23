# Spec: Cross-node networking with subnet persistence

Status: draft for review
Roadmap item: 1 (foundation for per-replica IP reporting, multi-replica, services)

## Problem

Every agent today loads its CNI bridge config from a static `/etc/cni/net.d`
conflist with host-local IPAM over the same `10.22.0.0/16` on every node. Two
consequences:

1. **IP collisions** — node A and node B both hand out `10.22.0.x`. Container
   IPs are not unique across the cluster.
2. **No cross-node routes** — even non-colliding IPs aren't reachable from
   another node; there is no route telling node A how to reach node B's
   container subnet.

Goal: each node gets a **unique, persistent subnet**; each node installs
**routes to every other node's subnet** via that peer's underlay host IP; and
**adding a node is automatic** — no manual conf or route edits.

## Design overview

```
Control plane                          Agent (each node)
-------------                          -----------------
SubnetAllocator (SQLite, persistent)   register() -> receives NetworkConfig
  node_id -> /24 subnet                build CNI bridge conf from subnet
                                       (gocni.WithConfListBytes, in-process)
registerNode -> allocate + return      route-sync loop (heartbeat cadence):
GET /nodes/{id}/routes -> peer subnets    GET /nodes/{id}/routes
                                          reconcile kernel routes via netlink
```

Underlay assumption (v1): all nodes share an L3 network where each node's
host IP can reach every other node's host IP directly (flat LAN / same VPC).
Overlay/encapsulation (VXLAN/WireGuard) is explicitly out of scope and noted
as a future option if nodes span L3 boundaries.

---

## Part 1 — Subnet allocation and persistence (control plane)

### 1a. Address math

- Cluster CIDR: `10.22.0.0/16` (configurable; default matches existing
  `runtime.BridgeSubnet`).
- Per-node block: `/24`. Node index `i` in `[0, 255]` →
  - subnet `10.22.i.0/24`
  - gateway (bridge IP on that host) `10.22.i.1`
  - 253 usable container IPs per node, up to 256 nodes.
- >256 nodes is out of scope; allocator returns an error when the pool is
  exhausted (documented limit, not a silent failure).

### 1b. Persistent store

New file `internal/reconciler/subnets_sqlite.go`, backed by the **same**
`/var/lib/smith/state.db` file, using the existing incremental-migration
discipline (base `CREATE TABLE IF NOT EXISTS` + `columnExists` guards).

```sql
CREATE TABLE IF NOT EXISTS node_subnets (
    node_id TEXT PRIMARY KEY,
    idx     INTEGER NOT NULL UNIQUE,
    subnet  TEXT NOT NULL
);
```

```go
type SubnetAllocator struct {
    db  *sql.DB
    mu  sync.Mutex // single-process serialization (pre-Raft)
    cidr string    // "10.22.0.0/16"
}

func NewSubnetAllocator(path, clusterCIDR string) (*SubnetAllocator, error)

// Allocate is idempotent: if node_id already has a row, it is returned
// unchanged (this is the persistence guarantee). Otherwise the lowest free
// index is allocated, persisted, and returned.
func (a *SubnetAllocator) Allocate(nodeID string) (types.NetworkConfig, error)

// Release frees a node's subnet. Called ONLY on explicit operator
// decommission, never on heartbeat timeout (see 1c).
func (a *SubnetAllocator) Release(nodeID string) error

// All returns node_id -> subnet for every allocation, used to build routes.
func (a *SubnetAllocator) All() (map[string]string, error)
```

`Allocate` holds `a.mu`, reads the existing row (return if present), else gap-
scans `idx` (lowest unused in `[0,255]`), `INSERT`s, returns
`NetworkConfig{Subnet, Gateway}`. The `UNIQUE(idx)` constraint is a
correctness backstop even though the mutex already serializes.

### 1c. Why subnets are NOT freed on heartbeat timeout (the persistence point)

The reconciler currently `registry.Remove`s a node after it misses its
heartbeat window and evicts its workloads. **Subnet allocation must not be
coupled to this.** A node that reboots or briefly partitions must get the
*same* subnet back, or:
- its bridge would be rebuilt on a different subnet while old routes linger;
- a different node could grab the freed `/24` and collide.

So: `Allocate` is keyed by node ID and survives control-plane restarts (it's
in SQLite, not the in-memory registry). Subnets are reclaimed only by an
explicit `DELETE /nodes/{id}` admin action that calls `Release`. At `/24`
granularity (256 slots) leaking a few decommissioned nodes is acceptable;
document manual decommission rather than auto-reclaim. This also sidesteps the
dead-node-reshuffle hazard entirely.

> This is the property that makes the pre-Raft control plane safe to restart.
> When Raft lands (roadmap item 6), the `node_subnets` table moves into the
> replicated log; the allocator API does not change.

---

## Part 2 — Data model (`internal/types/types.go`)

```go
// NetworkConfig is the per-node network assignment returned at registration.
type NetworkConfig struct {
    Subnet  string `json:"subnet"`  // "10.22.3.0/24"
    Gateway string `json:"gateway"` // "10.22.3.1"
}

// Route is one entry in a node's cross-node routing table.
type Route struct {
    Subnet string `json:"subnet"` // peer's container subnet, e.g. "10.22.4.0/24"
    Via    string `json:"via"`    // peer's underlay host IP, e.g. "192.168.1.56"
}
```

Add `HostIP string` to `Node`. It is the **underlay IP other nodes route
through**, which may differ from the API bind address. The agent populates it
from a new `-hostip` flag, defaulting to the host portion of `-addr`. The
control plane uses it as `Route.Via`.

---

## Part 3 — Control-plane wiring (`internal/api/server.go`, `cmd/server/main.go`)

### 3a. Allocator on the Server

Add `allocator *reconciler.SubnetAllocator` to `Server` with a setter
(consistent with `SetStatusFunc` / `SetAgentClient`):

```go
func (s *Server) SetSubnetAllocator(a *reconciler.SubnetAllocator)
```

`cmd/server/main.go` constructs it against `/var/lib/smith/state.db` and wires
it in before `server.Start()`.

### 3b. `registerNode` returns the allocation

After `s.registry.Register(node)`:

```go
netCfg, err := s.allocator.Allocate(node.ID)
if err != nil { httpError(w, ..., http.StatusInternalServerError); return }
writeJSON(w, http.StatusOK, netCfg)
```

(Currently returns empty 200; the agent ignores the body today, so this is
backward-additive.)

### 3c. New internal route endpoint (mTLS mux, not public)

```go
internalMux.HandleFunc("GET /nodes/{id}/routes", s.nodeRoutes)
```

`nodeRoutes` builds the routing table for the *requesting* node, excluding its
own subnet:

```go
func (s *Server) nodeRoutes(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    subnets, _ := s.allocator.All()       // nodeID -> subnet
    var routes []types.Route
    for nodeID, subnet := range subnets {
        if nodeID == id { continue }      // don't route to self
        peer, ok := s.registry.Get(nodeID)
        if !ok { continue }               // only route to known nodes
        routes = append(routes, types.Route{Subnet: subnet, Via: peer.HostIP})
    }
    writeJSON(w, http.StatusOK, routes)
}
```

Note: routes are only emitted for nodes currently in the registry — a
decommissioned node's subnet stops being advertised once it's removed, so
peers will prune the stale route on their next sync (Part 4c).

---

## Part 4 — Agent wiring (`internal/agent/agent.go`, `internal/runtime/`)

### 4a. Capture the assigned subnet at registration

`register()` decodes the `NetworkConfig` from the response and stores it on the
agent (`a.netCfg`).

### 4b. Build CNI conf dynamically from the subnet

Replace `NewCNI()`'s `c.Load(WithLoNetwork, WithDefaultConf)` with a variant
that takes the node subnet and loads an in-process conflist:

```go
func NewCNIForSubnet(subnet, gateway string) (*CNI, error) {
    confList := renderBridgeConflist(subnet, gateway) // []byte JSON
    c, err := gocni.New(... same opts ...)
    if err := c.Load(gocni.WithLoNetwork, gocni.WithConfListBytes(confList)); err != nil {
        return nil, err
    }
    return &CNI{cni: c}, nil
}
```

`renderBridgeConflist` produces a `bridge` + `host-local` + `portmap`
conflist with:
- `"subnet": <node subnet>`, `"gateway": <node gateway>` in host-local IPAM,
- `"isGateway": true`,
- **`"ipMasq": false`** — critical (see 4e); the bridge plugin must NOT
  blanket-masquerade, or cross-node container IPs would be SNAT'd to the host
  and real-IP routing breaks.

This removes the manual `/etc/cni/net.d` deployment step — the conf is now
generated from the control-plane-assigned subnet. `/opt/cni/bin` plugins are
still required.

### 4c. Route-sync loop (netlink)

New dependency `github.com/vishvananda/netlink` (manage routes via the kernel,
not by shelling `ip route`, consistent with using go-iptables over `ufw`).

A loop on the heartbeat cadence:
1. `GET https://<server>/nodes/<id>/routes` over mTLS → `[]types.Route`.
2. Reconcile against the kernel:
   - **Desired**: one route per entry, `Dst = route.Subnet`, `Gw = route.Via`.
   - **Existing smith-managed**: kernel routes whose `Dst` is *inside the
     cluster CIDR* but is *not this node's own subnet*. Only these are
     touched — host/default routes are never modified.
   - Add missing (`netlink.RouteReplace`), delete stale (`netlink.RouteDel`)
     for subnets no longer advertised (node removed).
3. Idempotent; safe to run every tick. New nodes appear automatically: when
   node D registers, every existing node's next sync pulls D's subnet and adds
   the route — no manual action. This is the "adding agents is automatic"
   requirement.

### 4d. Enable IP forwarding

The agent sets `net.ipv4.ip_forward=1` (write `/proc/sys/net/ipv4/ip_forward`)
on start, best-effort with a logged warning on failure. Without it the host
won't forward inter-node container traffic. Document as a host requirement;
the agent setting it makes deployment automatic.

### 4e. Masquerade only on cluster egress (`internal/runtime/firewall.go`)

With `ipMasq:false` on the bridge, add a single nat rule so containers can
still reach the internet while keeping real IPs cluster-internal:

```
iptables -t nat -A POSTROUTING -s 10.22.0.0/16 ! -d 10.22.0.0/16 -j MASQUERADE
```

Add `Firewall.EnsureMasquerade(clusterCIDR)` (idempotent, position-checked like
`EnsureForwarding`), called once on agent start. The existing
`EnsureForwarding` FORWARD ACCEPT rules for the `/16` already permit inter-node
forwarding in both directions — no change needed there, which is convenient:
the firewall foundation from the earlier task already covers it.

### 4f. Start() ordering (interacts with the ghost-cleanup task)

CNI can no longer be built in `agent.New` — it needs the subnet, which comes
from registration. Reorder `Start()`:

```
1. register()                       // obtain NetworkConfig
2. cni = NewCNIForSubnet(subnet,gw) // build bridge from assignment
3. firewall.EnsureForwarding(); firewall.EnsureMasquerade(clusterCIDR)
4. enable ip_forward
5. client.CleanupAll(cni)           // ghost cleanup — STILL needs CNI, now from step 2
6. go routeSyncLoop()
7. go heartbeatLoop()
8. go serveHTTP()
```

> This moves CNI init out of `New` and `CleanupAll` to after registration.
> The recently shipped fix that runs `CleanupAll(a.cni)` inside `Start()`
> stays — it just now uses the subnet-derived CNI built in step 2 instead of
> the `New`-time CNI. Flagging because it touches code we changed last session.

A wrinkle: today the agent serves assignments only after registering, but
cleanup currently runs before register. With networking, register must come
first (it yields the subnet). If registration fails at boot, the agent cannot
build its bridge — it should retry registration with backoff rather than
`log.Fatal`, so a control-plane restart doesn't permanently wedge agents.
(Recommend adding registration retry as part of this work.)

---

## Part 5 — New dependencies

- `github.com/vishvananda/netlink` — kernel route management.
- (go-cni already vendored; `WithConfListBytes` confirmed present in v1.1.10.)

## Part 6 — Testing / validation checklist

1. Two fresh agents register → each gets a distinct `/24` (`10.22.0.0/24`,
   `10.22.1.0/24`); verify rows in `node_subnets`.
2. Restart the control plane → re-registration returns the **same** subnets
   (persistence).
3. `ip route` on each node shows a route to the other's `/24` via its host IP;
   appears within one heartbeat interval.
4. Container on node A pings a container on node B by its real `10.22.x.y` IP.
5. Container on node A reaches the internet (egress MASQUERADE works).
6. `tcpdump` on node B confirms inter-node traffic arrives with node A's
   container IP as source (not SNAT'd).
7. Add a third agent → existing nodes pick up its route automatically with no
   manual step.
8. Decommission via `DELETE /nodes/{id}` → subnet released, peers prune the
   route on next sync.

## Decisions (locked 2026-06-22)

1. **Block size**: `/24` per node — 256 nodes, 253 container IPs each. Octet
   math: node index `i` → `10.22.i.0/24`, gateway `10.22.i.1`.
2. **HostIP source**: derive from the `-addr` host portion, with an optional
   `-hostip` flag override for when the API bind address ≠ the routing IP.
3. **Registration retry**: agent retries registration with backoff on boot
   instead of `log.Fatal`. Registration is now a hard dependency for bridge
   setup, so a control-plane restart must not permanently wedge agents.
4. **Decommission endpoint**: add `DELETE /nodes/{id}` (public, auth'd) in this
   work; it calls `registry.Remove` + `allocator.Release`. Peers prune the
   stale route on their next sync.
5. **Underlay**: flat L3 / LAN — each host IP reaches every other directly.
   Static routes via host IPs, no encapsulation. Overlay (VXLAN/WireGuard) is
   a documented future follow-on, not v1.
```
