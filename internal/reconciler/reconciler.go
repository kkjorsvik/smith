package reconciler

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	client    *runtime.Client
	store     Storer
	monitor   *health.Monitor
	registry  *registry.Registry
	scheduler *scheduler.Scheduler
	interval   time.Duration
	stop       chan struct{}
	pushed     map[string]pushRecord // workloadID -> last successful push
	mu         sync.Mutex
	httpClient *http.Client
}

// pushRecord tracks where and when a workload was last successfully pushed.
type pushRecord struct {
	nodeID   string
	pushedAt time.Time
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
	desired, err := r.store.List()
	if err != nil {
		return fmt.Errorf("list desired: %w", err)
	}

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

	// Ensure every desired workload is assigned and running on a node.
	for _, workload := range desired {
		assignment, err := r.scheduler.Assign(workload)
		if err != nil {
			log.Printf("reconciler: assign %s: %v", workload.ID, err)
			continue
		}

		node, exists := r.registry.Get(assignment.NodeID)
		if !exists {
			log.Printf("reconciler: node %s not found for workload %s", assignment.NodeID, workload.ID)
			r.scheduler.Unassign(workload.ID)
			continue
		}

		r.mu.Lock()
		rec, exists := r.pushed[workload.ID]
		alreadyPushed := exists && rec.nodeID == assignment.NodeID
		r.mu.Unlock()

		if !alreadyPushed {
			if err := r.pushAssignment(node, workload); err != nil {
				log.Printf("reconciler: push assignment %s to %s: %v", workload.ID, node.ID, err)
				continue
			}
			r.mu.Lock()
			r.pushed[workload.ID] = pushRecord{nodeID: assignment.NodeID, pushedAt: time.Now()}
			r.mu.Unlock()
			log.Printf("reconciler: pushed %s to %s", workload.ID, node.ID)
		}
	}

	// Check that pushed workloads are actually running on their assigned nodes.
	gracePeriod := 2 * r.interval
	for workloadID, rec := range r.pushed {
		// Give the container time to start before checking.
		if time.Since(rec.pushedAt) < gracePeriod {
			continue
		}

		node, exists := r.registry.Get(rec.nodeID)
		if !exists {
			continue
		}

		agentStatus, err := r.fetchAgentStatus(node)
		if err != nil {
			log.Printf("reconciler: fetch status from %s: %v", node.ID, err)
			continue
		}

		status, running := agentStatus[workloadID]
		if !running || status.Status != containerd.Running {
			log.Printf("reconciler: %s not running on %s, will repush", workloadID, rec.nodeID)
			r.mu.Lock()
			delete(r.pushed, workloadID)
			r.mu.Unlock()
		}
	}

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
