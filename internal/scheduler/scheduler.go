package scheduler

import (
	"fmt"
	"sync"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// Scheduler assigns workloads to nodes.
type Scheduler struct {
	registry    *registry.Registry
	mu          sync.RWMutex
	assignments map[string]string // workloadID -> nodeID
}

// New returns a Scheduler backed by the given registry.
func New(reg *registry.Registry) *Scheduler {
	return &Scheduler{
		registry:    reg,
		assignments: make(map[string]string),
	}
}

// Assign picks a node for the given workload and records the assignment.
// Returns an error if no alive nodes are available.
// If the workload is already assigned, returns the existing assignment.
func (s *Scheduler) Assign(w types.Workload) (types.Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already assigned — return existing assignment.
	if nodeID, exists := s.assignments[w.ID]; exists {
		return types.Assignment{WorkloadID: w.ID, NodeID: nodeID}, nil
	}

	nodes := s.registry.Alive()
	if len(nodes) == 0 {
		return types.Assignment{}, fmt.Errorf("no alive nodes available")
	}

	// Count assignments per node.
	counts := make(map[string]int, len(nodes))
	for _, n := range nodes {
		counts[n.ID] = 0
	}
	for _, nodeID := range s.assignments {
		counts[nodeID]++
	}

	// Pick the node with the fewest assignments.
	best := nodes[0]
	for _, n := range nodes[1:] {
		if counts[n.ID] < counts[best.ID] {
			best = n
		}
	}

	s.assignments[w.ID] = best.ID
	return types.Assignment{WorkloadID: w.ID, NodeID: best.ID}, nil
}

// Unassign removes the assignment for a workload.
func (s *Scheduler) Unassign(workloadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.assignments, workloadID)
}

// GetAssignment returns the current assignment for a workload.
func (s *Scheduler) GetAssignment(workloadID string) (types.Assignment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodeID, exists := s.assignments[workloadID]
	if !exists {
		return types.Assignment{}, false
	}
	return types.Assignment{WorkloadID: workloadID, NodeID: nodeID}, true
}

// ListAssignments returns all current workload->node assignments.
func (s *Scheduler) ListAssignments() []types.Assignment {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]types.Assignment, 0, len(s.assignments))
	for wID, nID := range s.assignments {
		out = append(out, types.Assignment{WorkloadID: wID, NodeID: nID})
	}
	return out
}

// ReassignNode moves all workloads from a dead node back into unassigned
// state so the scheduler will place them on a healthy node next cycle.
func (s *Scheduler) ReassignNode(nodeID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var evicted []string
	for wID, nID := range s.assignments {
		if nID == nodeID {
			delete(s.assignments, wID)
			evicted = append(evicted, wID)
		}
	}
	return evicted
}
