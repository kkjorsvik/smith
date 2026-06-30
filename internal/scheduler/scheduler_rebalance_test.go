package scheduler

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// A cluster already packed best-fit needs no moves.
func TestRebalancePlanEmptyWhenBalanced(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 4000})
	reg.Register(types.Node{ID: "n2", CPU: 4, MemoryMB: 4000})
	s := New(reg)
	if _, err := s.Assign("a-0", "a", types.Resources{MemoryMB: 500}); err != nil {
		t.Fatalf("assign a: %v", err)
	}
	if _, err := s.Assign("b-0", "b", types.Resources{MemoryMB: 500}); err != nil {
		t.Fatalf("assign b: %v", err)
	}
	if moves := s.RebalancePlan(); len(moves) != 0 {
		t.Fatalf("balanced cluster should need no moves, got %v", moves)
	}
}

// The core scenario: best-fit packed everything onto one node, that node then
// rejoins reporting less RAM (now overcommitted), and rebalance drains the
// excess onto the empty nodes — without mutating until committed, and
// converging (idempotent) afterwards.
func TestRebalanceDrainsOvercommittedNode(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 8000}) // cap 6800
	reg.Register(types.Node{ID: "n2", CPU: 4, MemoryMB: 8000})
	reg.Register(types.Node{ID: "n3", CPU: 4, MemoryMB: 8000})
	s := New(reg)

	ids := []string{"a", "b", "c", "d", "e", "f"}
	for _, id := range ids {
		if _, err := s.Assign(id+"-0", id, types.Resources{MemoryMB: 1000}); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}
	// All six packed onto n1 (best-fit + deterministic lowest-id), 6000 < 6800.
	// n1 rejoins smaller: cap drops to 4250, below the 6000 committed.
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 5000})

	plan := s.RebalancePlan()
	if len(plan) == 0 {
		t.Fatal("expected moves to drain the overcommitted node")
	}
	for _, m := range plan {
		if m.FromNode != "n1" {
			t.Fatalf("move %+v should originate from overcommitted n1", m)
		}
		if m.ToNode == "n1" {
			t.Fatalf("move %+v should leave n1", m)
		}
	}

	// RebalancePlan is pure: nothing has moved yet.
	for _, id := range ids {
		a, _ := s.GetAssignment(id + "-0")
		if a.NodeID != "n1" {
			t.Fatalf("RebalancePlan must not mutate; %s-0 now on %s", id, a.NodeID)
		}
	}

	// Commit and verify the node is no longer overcommitted.
	committed := s.Rebalance()
	if len(committed) != len(plan) {
		t.Fatalf("Rebalance enacted %d moves, plan had %d", len(committed), len(plan))
	}
	if over := s.Overcommitted(); len(over) != 0 {
		t.Fatalf("after rebalance no node should be overcommitted, got %+v", over)
	}

	// Idempotent: a second plan is empty.
	if plan2 := s.RebalancePlan(); len(plan2) != 0 {
		t.Fatalf("rebalance should converge; second plan still has moves: %v", plan2)
	}
}
