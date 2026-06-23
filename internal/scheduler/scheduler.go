package scheduler

import (
	"fmt"
	"sync"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// Scheduler assigns workload replicas to nodes.
type Scheduler struct {
	registry    *registry.Registry
	mu          sync.RWMutex
	assignments map[string]string // replicaID -> nodeID
	parents     map[string]string // replicaID -> parent workloadID
}

// New returns a Scheduler backed by the given registry.
func New(reg *registry.Registry) *Scheduler {
	return &Scheduler{
		registry:    reg,
		assignments: make(map[string]string),
		parents:     make(map[string]string),
	}
}

// Assign picks a node for the given replica and records the assignment.
// replicaID is the unique instance ID (e.g. "smith-nginx-0"); parentID is the
// workload it belongs to, used to spread sibling replicas across nodes.
// Returns an error if no alive nodes are available. If the replica is already
// assigned, returns the existing assignment (sticky).
func (s *Scheduler) Assign(replicaID, parentID string) (types.Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already assigned — return existing assignment.
	if nodeID, exists := s.assignments[replicaID]; exists {
		return types.Assignment{WorkloadID: replicaID, NodeID: nodeID, ParentID: s.parents[replicaID]}, nil
	}

	nodes := s.registry.Alive()
	if len(nodes) == 0 {
		return types.Assignment{}, fmt.Errorf("no alive nodes available")
	}

	// Per alive node: total load and count of siblings (same parent).
	load := make(map[string]int, len(nodes))
	siblings := make(map[string]int, len(nodes))
	for _, n := range nodes {
		load[n.ID] = 0
		siblings[n.ID] = 0
	}
	for rID, nodeID := range s.assignments {
		load[nodeID]++
		if s.parents[rID] == parentID {
			siblings[nodeID]++
		}
	}

	// Anti-affinity: pick the node with the fewest siblings of this workload,
	// breaking ties by fewest total assignments. This spreads replicas
	// one-per-node while nodes are available, then stacks on least-loaded.
	best := nodes[0]
	for _, n := range nodes[1:] {
		if siblings[n.ID] < siblings[best.ID] ||
			(siblings[n.ID] == siblings[best.ID] && load[n.ID] < load[best.ID]) {
			best = n
		}
	}

	s.assignments[replicaID] = best.ID
	s.parents[replicaID] = parentID
	return types.Assignment{WorkloadID: replicaID, NodeID: best.ID, ParentID: parentID}, nil
}

// Unassign removes the assignment for a replica.
func (s *Scheduler) Unassign(replicaID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.assignments, replicaID)
	delete(s.parents, replicaID)
}

// GetAssignment returns the current assignment for a replica.
func (s *Scheduler) GetAssignment(replicaID string) (types.Assignment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodeID, exists := s.assignments[replicaID]
	if !exists {
		return types.Assignment{}, false
	}
	return types.Assignment{WorkloadID: replicaID, NodeID: nodeID, ParentID: s.parents[replicaID]}, true
}

// ListAssignments returns all current replica->node assignments.
func (s *Scheduler) ListAssignments() []types.Assignment {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]types.Assignment, 0, len(s.assignments))
	for rID, nID := range s.assignments {
		out = append(out, types.Assignment{WorkloadID: rID, NodeID: nID, ParentID: s.parents[rID]})
	}
	return out
}

// ReassignNode moves all replicas from a dead node back into unassigned
// state so the scheduler will place them on a healthy node next cycle.
func (s *Scheduler) ReassignNode(nodeID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var evicted []string
	for rID, nID := range s.assignments {
		if nID == nodeID {
			delete(s.assignments, rID)
			delete(s.parents, rID)
			evicted = append(evicted, rID)
		}
	}
	return evicted
}
