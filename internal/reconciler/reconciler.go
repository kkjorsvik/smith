package reconciler

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/containerd/containerd"
	"github.com/kkjorsvik/smith/internal/health"
	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/scheduler"
	"github.com/kkjorsvik/smith/internal/types"
)

// Storer is the desired-state backend the reconciler drives toward.
// Both the in-memory Store and the SQLiteStore satisfy it.
type Storer interface {
	Add(w types.Workload) error
	Remove(id string) error
	List() (map[string]types.Workload, error)
}

// Reconciler periodically reconciles desired workloads against the cluster:
// it assigns each workload to a node via the scheduler, pushes the assignment
// to that node's agent over HTTP, and evicts workloads from dead nodes. The
// agents own container lifecycle locally; the control plane only schedules.
type Reconciler struct {
	client     *runtime.Client
	store      Storer
	monitor    *health.Monitor
	registry   *registry.Registry
	scheduler  *scheduler.Scheduler
	interval   time.Duration
	stop       chan struct{}
	pushed     map[string]pushRecord // workloadID -> last successful push
	mu         sync.Mutex
	httpClient *http.Client
}

// pushRecord tracks where, when, and with what spec a replica was last pushed.
type pushRecord struct {
	nodeID   string
	pushedAt time.Time
	specHash string
}

// New returns a Reconciler that reconciles every interval. tlsCfg is used
// for mTLS calls to agents (assign/unassign/status).
func New(client *runtime.Client, store Storer, monitor *health.Monitor, reg *registry.Registry, sched *scheduler.Scheduler, tlsCfg *tls.Config, interval time.Duration) *Reconciler {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}
	return &Reconciler{
		client:     client,
		store:      store,
		monitor:    monitor,
		registry:   reg,
		scheduler:  sched,
		interval:   interval,
		stop:       make(chan struct{}),
		pushed:     make(map[string]pushRecord),
		httpClient: httpClient,
	}
}

// Start launches the reconcile loop in a goroutine.
func (r *Reconciler) Start() {
	go r.loop()
}

// Stop halts the reconcile loop.
func (r *Reconciler) Stop() {
	close(r.stop)
}

func (r *Reconciler) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	if err := r.reconcile(); err != nil {
		log.Printf("reconciler: %v", err)
	}
	for {
		select {
		case <-ticker.C:
			if err := r.reconcile(); err != nil {
				log.Printf("reconciler: %v", err)
			}
		case <-r.stop:
			return
		}
	}
}

func (r *Reconciler) reconcile() error {
	stored, err := r.store.List()
	if err != nil {
		return fmt.Errorf("list desired: %w", err)
	}
	// Expand each workload into its replica instances; the rest of the loop
	// drives the cluster toward this per-replica desired set.
	desired := expand(stored)

	// Surface any node whose committed requests exceed its schedulable
	// capacity. Best-fit placement prevents this at assignment time, so a
	// warning here means a node rejoined with less CPU/memory than the load
	// already on it — worth seeing rather than inferring from a UI table.
	r.logOvercommit()

	// Detect dead nodes and evict their workloads back to unassigned.
	for _, node := range r.registry.Dead() {
		evicted := r.scheduler.ReassignNode(node.ID)
		if len(evicted) > 0 {
			log.Printf("reconciler: node %s is dead, evicting %d workloads", node.ID, len(evicted))
			r.mu.Lock()
			for _, wID := range evicted {
				delete(r.pushed, wID)
			}
			r.mu.Unlock()
		}
		r.registry.Remove(node.ID)
	}

	// Snapshot which replicas are currently Running across the cluster, used
	// both for rollout pacing and the repush-if-not-running check below.
	running := r.clusterRunning()

	// Per-workload count of replicas that are not Running (down or starting).
	// A rolling update must not push this above the workload's MaxUnavailable.
	unavailable := make(map[string]int)
	for _, inst := range desired {
		if !running[inst.wl.ID] {
			unavailable[inst.parent]++
		}
	}

	// Ensure every desired replica is assigned and on its node with the
	// current spec. Process in stable replica order so rollouts are
	// deterministic.
	for _, id := range sortedReplicaIDs(desired) {
		inst := desired[id]

		assignment, err := r.scheduler.Assign(inst.wl.ID, inst.parent, requestOf(inst.wl))
		if err != nil {
			log.Printf("reconciler: assign %s: %v", inst.wl.ID, err)
			continue
		}

		node, exists := r.registry.Get(assignment.NodeID)
		if !exists {
			log.Printf("reconciler: node %s not found for replica %s", assignment.NodeID, inst.wl.ID)
			r.scheduler.Unassign(inst.wl.ID)
			continue
		}

		desiredHash := specHash(inst.wl)

		r.mu.Lock()
		rec, pushedExists := r.pushed[inst.wl.ID]
		r.mu.Unlock()

		switch {
		case !pushedExists:
			// Initial placement.
			if err := r.pushAssignment(node, inst.wl); err != nil {
				log.Printf("reconciler: push assignment %s to %s: %v", inst.wl.ID, node.ID, err)
				continue
			}
			r.recordPush(inst.wl.ID, assignment.NodeID, desiredHash)
			log.Printf("reconciler: pushed %s to %s", inst.wl.ID, node.ID)

		case rec.nodeID != assignment.NodeID:
			// The replica moved to a new node (dead-node eviction, or a re-fit
			// after its request outgrew the old node). Stop the old container
			// before starting the new one so we never run two copies.
			if err := r.relocate(rec.nodeID, node, inst.wl); err != nil {
				log.Printf("reconciler: relocate %s to %s: %v", inst.wl.ID, node.ID, err)
				continue
			}
			r.recordPush(inst.wl.ID, assignment.NodeID, desiredHash)
			log.Printf("reconciler: relocated %s to %s (was on %s)", inst.wl.ID, node.ID, rec.nodeID)

		case rec.specHash != desiredHash:
			// Stale spec — roll it, but only a Running replica and only while
			// the workload stays within its MaxUnavailable budget. Replacing
			// it makes it temporarily unavailable, so account for that.
			if running[inst.wl.ID] && unavailable[inst.parent] < maxUnavailable(stored[inst.parent]) {
				if err := r.pushUnassign(node, inst.wl.ID); err != nil {
					log.Printf("reconciler: roll unassign %s on %s: %v", inst.wl.ID, node.ID, err)
					continue
				}
				if err := r.pushAssignment(node, inst.wl); err != nil {
					log.Printf("reconciler: roll assign %s to %s: %v", inst.wl.ID, node.ID, err)
					continue
				}
				r.recordPush(inst.wl.ID, assignment.NodeID, desiredHash)
				unavailable[inst.parent]++
				log.Printf("reconciler: rolled %s on %s to new spec", inst.wl.ID, node.ID)
			}

		default:
			// Up to date — nothing to do.
		}
	}

	// Repush replicas that are no longer Running (past a grace period so a
	// freshly-pushed container has time to start). The next reconcile places
	// them again with the current desired spec.
	gracePeriod := 2 * r.interval
	r.mu.Lock()
	for replicaID, rec := range r.pushed {
		if time.Since(rec.pushedAt) < gracePeriod {
			continue
		}
		if !running[replicaID] {
			log.Printf("reconciler: %s not running on %s, will repush", replicaID, rec.nodeID)
			delete(r.pushed, replicaID)
		}
	}
	r.mu.Unlock()

	// Stop containers on nodes for workloads no longer in desired state.
	for _, assignment := range r.scheduler.ListAssignments() {
		if _, exists := desired[assignment.WorkloadID]; !exists {
			node, ok := r.registry.Get(assignment.NodeID)
			if ok {
				if err := r.pushUnassign(node, assignment.WorkloadID); err != nil {
					log.Printf("reconciler: unassign %s from %s: %v", assignment.WorkloadID, node.ID, err)
				}
			}
			r.scheduler.Unassign(assignment.WorkloadID)
			r.mu.Lock()
			delete(r.pushed, assignment.WorkloadID)
			r.mu.Unlock()
		}
	}

	return nil
}

// replicaInstance is one replica of a workload: a derived single-container
// workload (ID = "<parent>-<index>") plus the parent workload ID.
type replicaInstance struct {
	wl     types.Workload
	parent string
}

// expand turns each desired workload into its replica instances, keyed by
// replica ID. A workload with Replicas < 1 yields a single replica "-0".
func expand(desired map[string]types.Workload) map[string]replicaInstance {
	out := make(map[string]replicaInstance)
	for id, w := range desired {
		n := w.Replicas
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			rw := w
			rw.ID = fmt.Sprintf("%s-%d", id, i)
			rw.Replicas = 0 // a replica instance is a single container
			out[rw.ID] = replicaInstance{wl: rw, parent: id}
		}
	}
	return out
}

// sortedReplicaIDs returns the desired replica IDs in stable (sorted) order so
// rollouts replace replicas deterministically.
func sortedReplicaIDs(desired map[string]replicaInstance) []string {
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// relocate moves a replica from its old node to newNode: it stops the old
// container first (if the old node is still registered) so a move never leaves
// a duplicate — important for volume-backed workloads, where two live copies
// would write the same data. A failure to stop the old container is logged but
// not fatal; the start on the new node is what must succeed.
func (r *Reconciler) relocate(oldNodeID string, newNode types.Node, w types.Workload) error {
	if oldNode, ok := r.registry.Get(oldNodeID); ok {
		if err := r.pushUnassign(oldNode, w.ID); err != nil {
			log.Printf("reconciler: stop moved %s on old node %s: %v", w.ID, oldNodeID, err)
		}
	}
	return r.pushAssignment(newNode, w)
}

// logOvercommit warns for each node whose committed resource requests exceed
// its schedulable capacity.
func (r *Reconciler) logOvercommit() {
	for _, n := range r.scheduler.Overcommitted() {
		log.Printf("reconciler: node %s overcommitted: cpu %d/%dm mem %d/%dMB",
			n.NodeID, n.CPUMillicores, n.CapCPU, n.MemoryMB, n.CapMemoryMB)
	}
}

// requestOf returns a workload's resource request for scheduling (its limits;
// the zero value when it declares no Resources).
func requestOf(w types.Workload) types.Resources {
	if w.Resources == nil {
		return types.Resources{}
	}
	return *w.Resources
}

// maxUnavailable returns the workload's rolling-update budget, defaulting to 1.
func maxUnavailable(w types.Workload) int {
	if w.MaxUnavailable < 1 {
		return 1
	}
	return w.MaxUnavailable
}

// specHash is a stable digest of the container-defining fields of a workload.
// It deliberately excludes Replicas and MaxUnavailable, so scaling and changing
// the rollout budget do not trigger a rolling update.
func specHash(w types.Workload) string {
	canonical := struct {
		Image     string              `json:"image"`
		Args      []string            `json:"args"`
		Env       map[string]string   `json:"env"`
		Ports     []types.PortMapping `json:"ports"`
		Resources *types.Resources    `json:"resources"`
		Volumes   []types.Volume      `json:"volumes"`
	}{w.Image, w.Args, w.Env, w.Ports, w.Resources, w.Volumes}

	// encoding/json sorts map keys, so Env ordering is stable.
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// recordPush stores the push record for a replica under the lock.
func (r *Reconciler) recordPush(replicaID, nodeID, specHash string) {
	r.mu.Lock()
	r.pushed[replicaID] = pushRecord{nodeID: nodeID, pushedAt: time.Now(), specHash: specHash}
	r.mu.Unlock()
}

// clusterRunning returns a map of replica ID -> whether it is Running, built
// from a single status fetch per alive node.
func (r *Reconciler) clusterRunning() map[string]bool {
	out := make(map[string]bool)
	for _, node := range r.registry.Alive() {
		status, err := r.fetchAgentStatus(node)
		if err != nil {
			log.Printf("reconciler: fetch status from %s: %v", node.ID, err)
			continue
		}
		for id, cs := range status {
			if cs.Status == containerd.Running {
				out[id] = true
			}
		}
	}
	return out
}

// pushAssignment sends a workload assignment to an agent node.
func (r *Reconciler) pushAssignment(node types.Node, w types.Workload) error {
	body, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("marshal workload: %w", err)
	}

	url := fmt.Sprintf("https://%s/assign", node.Addr)
	resp, err := r.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("post assign: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("assign failed: status %d", resp.StatusCode)
	}

	return nil
}

// pushUnassign tells an agent node to stop a container.
func (r *Reconciler) pushUnassign(node types.Node, workloadID string) error {
	url := fmt.Sprintf("https://%s/assign/%s", node.Addr, workloadID)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete assign: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unassign failed: status %d", resp.StatusCode)
	}

	return nil
}

// fetchAgentStatus queries an agent's /status endpoint and returns
// a map of container ID -> ContainerStatus.
func (r *Reconciler) fetchAgentStatus(node types.Node) (map[string]runtime.ContainerStatus, error) {
	url := fmt.Sprintf("https://%s/status", node.Addr)
	resp, err := r.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get status from %s: %w", node.ID, err)
	}
	defer resp.Body.Close()

	var status map[string]runtime.ContainerStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode status from %s: %w", node.ID, err)
	}

	return status, nil
}

// AggregateStatus fans out to every alive node's /status endpoint and
// returns a map of nodeID -> (containerID -> ContainerStatus).
func (r *Reconciler) AggregateStatus() map[string]map[string]runtime.ContainerStatus {
	out := make(map[string]map[string]runtime.ContainerStatus)

	for _, node := range r.registry.Alive() {
		status, err := r.fetchAgentStatus(node)
		if err != nil {
			log.Printf("reconciler: aggregate status from %s: %v", node.ID, err)
			out[node.ID] = map[string]runtime.ContainerStatus{}
			continue
		}
		out[node.ID] = status
	}

	return out
}
