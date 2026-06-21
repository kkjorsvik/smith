package reconciler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

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
	interval  time.Duration
	stop      chan struct{}
}

// New returns a Reconciler that reconciles every interval.
func New(client *runtime.Client, store Storer, monitor *health.Monitor, reg *registry.Registry, sched *scheduler.Scheduler, interval time.Duration) *Reconciler {
	return &Reconciler{
		client:    client,
		store:     store,
		monitor:   monitor,
		registry:  reg,
		scheduler: sched,
		interval:  interval,
		stop:      make(chan struct{}),
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

		if err := r.pushAssignment(node, workload); err != nil {
			log.Printf("reconciler: push assignment %s to %s: %v", workload.ID, node.ID, err)
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

	url := fmt.Sprintf("http://%s/assign", node.Addr)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
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
	url := fmt.Sprintf("http://%s/assign/%s", node.Addr, workloadID)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete assign: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unassign failed: status %d", resp.StatusCode)
	}

	return nil
}
