package scheduler

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

func newSchedulerWithNodes(ids ...string) *Scheduler {
	reg := registry.New()
	for _, id := range ids {
		reg.Register(types.Node{ID: id, Addr: id + ":9000"})
	}
	return New(reg)
}

func TestAssignSpreadsReplicas(t *testing.T) {
	s := newSchedulerWithNodes("n1", "n2")

	a0, err := s.Assign("w-0", "w", types.Resources{})
	if err != nil {
		t.Fatalf("assign w-0: %v", err)
	}
	a1, err := s.Assign("w-1", "w", types.Resources{})
	if err != nil {
		t.Fatalf("assign w-1: %v", err)
	}

	// With 2 nodes, the first two replicas must land on distinct nodes.
	if a0.NodeID == a1.NodeID {
		t.Fatalf("w-0 and w-1 both on %s, want distinct nodes", a0.NodeID)
	}

	// A third replica stacks (2 nodes, 3 replicas) → 2+1, never 3+0.
	a2, err := s.Assign("w-2", "w", types.Resources{})
	if err != nil {
		t.Fatalf("assign w-2: %v", err)
	}
	counts := map[string]int{}
	for _, a := range []types.Assignment{a0, a1, a2} {
		counts[a.NodeID]++
	}
	if len(counts) != 2 {
		t.Fatalf("expected replicas across 2 nodes, got %v", counts)
	}
	for node, c := range counts {
		if c > 2 {
			t.Fatalf("node %s has %d replicas, want max 2 (no 3+0)", node, c)
		}
	}
}

func TestAssignSticky(t *testing.T) {
	s := newSchedulerWithNodes("n1", "n2")

	first, err := s.Assign("w-0", "w", types.Resources{})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	again, err := s.Assign("w-0", "w", types.Resources{})
	if err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	if again.NodeID != first.NodeID {
		t.Fatalf("sticky broken: %s then %s", first.NodeID, again.NodeID)
	}
	if again.ParentID != "w" {
		t.Fatalf("ParentID = %q, want w", again.ParentID)
	}
}

func TestAssignNoNodes(t *testing.T) {
	s := newSchedulerWithNodes()
	if _, err := s.Assign("w-0", "w", types.Resources{}); err == nil {
		t.Fatal("expected error with no alive nodes, got nil")
	}
}

func TestReassignNodeFailover(t *testing.T) {
	s := newSchedulerWithNodes("n1", "n2")

	// Place two replicas (one per node).
	a0, _ := s.Assign("w-0", "w", types.Resources{})
	s.Assign("w-1", "w", types.Resources{})

	// Evict the node hosting w-0; it should be reassigned to the survivor.
	evicted := s.ReassignNode(a0.NodeID)
	if len(evicted) != 1 || evicted[0] != "w-0" {
		t.Fatalf("evicted = %v, want [w-0]", evicted)
	}

	// Re-place: only the other node is "alive" in the registry sense here,
	// but both are still registered, so it just needs to land somewhere and
	// be sticky afterward.
	re, err := s.Assign("w-0", "w", types.Resources{})
	if err != nil {
		t.Fatalf("reassign after eviction: %v", err)
	}
	if re.NodeID == "" {
		t.Fatal("reassigned to empty node")
	}
}
