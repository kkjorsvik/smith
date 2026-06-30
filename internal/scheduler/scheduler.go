package scheduler

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// schedulableReserveFraction is the share of each node's CPU and memory held
// back from scheduling for the OS and containerd.
const schedulableReserveFraction = 0.15

// Scheduler assigns workload replicas to nodes, resource-aware: a replica only
// lands on a node with enough free CPU and memory, packed best-fit.
type Scheduler struct {
	registry    *registry.Registry
	mu          sync.RWMutex
	assignments map[string]string          // replicaID -> nodeID
	parents     map[string]string          // replicaID -> parent workloadID
	requests    map[string]types.Resources // replicaID -> resource request
}

// New returns a Scheduler backed by the given registry.
func New(reg *registry.Registry) *Scheduler {
	return &Scheduler{
		registry:    reg,
		assignments: make(map[string]string),
		parents:     make(map[string]string),
		requests:    make(map[string]types.Resources),
	}
}

// schedulable returns a node's schedulable CPU (millicores) and memory (MB)
// after the system reservation.
func schedulable(n types.Node) (cpu, mem int) {
	cpu = int(float64(n.CPU*1000) * (1 - schedulableReserveFraction))
	mem = int(float64(n.MemoryMB) * (1 - schedulableReserveFraction))
	return cpu, mem
}

// Assign picks a node for the given replica and records the assignment.
// replicaID is the unique instance ID (e.g. "smith-nginx-0"); parentID is the
// workload it belongs to (used to spread siblings); req is the replica's
// resource request. Placement filters to nodes that fit the request, keeps
// anti-affinity primary, and packs best-fit. Returns an error if no node fits
// (the replica stays pending). An already-assigned replica is sticky.
func (s *Scheduler) Assign(replicaID, parentID string, req types.Resources) (types.Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already assigned — sticky, but only while the replica still fits its
	// node. Refresh the recorded request and keep it put if the (possibly
	// grown) request still fits; otherwise release it here and fall through to
	// re-place it, so a grown request or a shrunk node can't silently
	// overcommit the node.
	if nodeID, exists := s.assignments[replicaID]; exists {
		if s.fitsOnNode(nodeID, replicaID, req) {
			s.requests[replicaID] = req
			return types.Assignment{WorkloadID: replicaID, NodeID: nodeID, ParentID: s.parents[replicaID]}, nil
		}
		delete(s.assignments, replicaID)
		delete(s.requests, replicaID)
		delete(s.parents, replicaID)
	}

	nodes := s.registry.Alive()
	if len(nodes) == 0 {
		return types.Assignment{}, fmt.Errorf("no alive nodes available")
	}

	// Per-node allocated resources and sibling counts.
	allocCPU := make(map[string]int)
	allocMem := make(map[string]int)
	siblings := make(map[string]int)
	for rID, nodeID := range s.assignments {
		r := s.requests[rID]
		allocCPU[nodeID] += r.CPUMillicores
		allocMem[nodeID] += r.MemoryMB
		if s.parents[rID] == parentID {
			siblings[nodeID]++
		}
	}

	// Candidate nodes that fit the request, scored by remaining free fraction
	// after placement (smaller = tighter pack = best-fit).
	type candidate struct {
		id        string
		siblings  int
		remaining float64
	}
	var fitting []candidate
	for _, n := range nodes {
		capCPU, capMem := schedulable(n)
		if allocCPU[n.ID]+req.CPUMillicores > capCPU || allocMem[n.ID]+req.MemoryMB > capMem {
			continue // doesn't fit
		}
		freeCPU, freeMem := 0.0, 0.0
		if capCPU > 0 {
			freeCPU = float64(capCPU-allocCPU[n.ID]-req.CPUMillicores) / float64(capCPU)
		}
		if capMem > 0 {
			freeMem = float64(capMem-allocMem[n.ID]-req.MemoryMB) / float64(capMem)
		}
		fitting = append(fitting, candidate{id: n.ID, siblings: siblings[n.ID], remaining: freeCPU + freeMem})
	}
	if len(fitting) == 0 {
		return types.Assignment{}, fmt.Errorf("insufficient capacity for %s (cpu=%dm mem=%dMB)", replicaID, req.CPUMillicores, req.MemoryMB)
	}

	// Anti-affinity primary (fewest siblings), best-fit tiebreak (least
	// remaining capacity that still fits).
	best := fitting[0]
	for _, c := range fitting[1:] {
		if c.siblings < best.siblings ||
			(c.siblings == best.siblings && c.remaining < best.remaining) {
			best = c
		}
	}

	s.assignments[replicaID] = best.id
	s.parents[replicaID] = parentID
	s.requests[replicaID] = req
	return types.Assignment{WorkloadID: replicaID, NodeID: best.id, ParentID: parentID}, nil
}

// NodeLoad reports a node's committed resource requests against its schedulable
// capacity.
type NodeLoad struct {
	NodeID        string
	CPUMillicores int // committed
	MemoryMB      int // committed
	CapCPU        int // schedulable
	CapMemoryMB   int // schedulable
}

// fitsOnNode reports whether req fits on nodeID, counting everything already
// committed there except replicaID's own current assignment. A node that is no
// longer registered never fits. Callers must hold s.mu.
func (s *Scheduler) fitsOnNode(nodeID, replicaID string, req types.Resources) bool {
	node, ok := s.registry.Get(nodeID)
	if !ok {
		return false
	}
	capCPU, capMem := schedulable(node)
	usedCPU, usedMem := 0, 0
	for rID, nID := range s.assignments {
		if nID != nodeID || rID == replicaID {
			continue
		}
		r := s.requests[rID]
		usedCPU += r.CPUMillicores
		usedMem += r.MemoryMB
	}
	return usedCPU+req.CPUMillicores <= capCPU && usedMem+req.MemoryMB <= capMem
}

// Overcommitted returns nodes whose committed CPU or memory requests exceed
// their schedulable capacity, sorted by node ID for stable output. This is a
// safety net: best-fit placement prevents overcommit at assignment time, but a
// node can still end up over capacity if it rejoins reporting less CPU/memory
// than the load already placed on it.
func (s *Scheduler) Overcommitted() []NodeLoad {
	s.mu.RLock()
	defer s.mu.RUnlock()

	usedCPU := make(map[string]int)
	usedMem := make(map[string]int)
	for rID, nodeID := range s.assignments {
		r := s.requests[rID]
		usedCPU[nodeID] += r.CPUMillicores
		usedMem[nodeID] += r.MemoryMB
	}

	var out []NodeLoad
	for nodeID := range usedCPU {
		node, ok := s.registry.Get(nodeID)
		if !ok {
			continue
		}
		capCPU, capMem := schedulable(node)
		if usedCPU[nodeID] > capCPU || usedMem[nodeID] > capMem {
			out = append(out, NodeLoad{
				NodeID:        nodeID,
				CPUMillicores: usedCPU[nodeID],
				MemoryMB:      usedMem[nodeID],
				CapCPU:        capCPU,
				CapMemoryMB:   capMem,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Unassign removes the assignment for a replica.
func (s *Scheduler) Unassign(replicaID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.assignments, replicaID)
	delete(s.parents, replicaID)
	delete(s.requests, replicaID)
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
			delete(s.requests, rID)
			evicted = append(evicted, rID)
		}
	}
	return evicted
}
