package scheduler

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// schedulable on a node with MemoryMB=1000 is 850 (15% reserve).
func TestAssignRejectsWhenNoFit(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 1, MemoryMB: 1000})
	s := New(reg)

	// 900MB exceeds the ~850MB schedulable capacity → pending.
	if _, err := s.Assign("big-0", "big", types.Resources{MemoryMB: 900}); err == nil {
		t.Fatal("expected insufficient-capacity error for 900MB on an 850MB-schedulable node")
	}

	// 500MB fits.
	if _, err := s.Assign("a-0", "a", types.Resources{MemoryMB: 500}); err != nil {
		t.Fatalf("500MB should fit: %v", err)
	}
	// A second 500MB would total 1000 > 850 → no fit.
	if _, err := s.Assign("b-0", "b", types.Resources{MemoryMB: 500}); err == nil {
		t.Fatal("expected no-fit for a second 500MB request")
	}
}

func TestAssignBestFitConsolidates(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 2, MemoryMB: 2000})
	reg.Register(types.Node{ID: "n2", CPU: 2, MemoryMB: 2000})
	s := New(reg)

	// Two replicas of *different* workloads (no anti-affinity between them).
	// Best-fit should pack the second onto the same node as the first.
	a, err := s.Assign("a-0", "a", types.Resources{MemoryMB: 400})
	if err != nil {
		t.Fatalf("assign a-0: %v", err)
	}
	b, err := s.Assign("b-0", "b", types.Resources{MemoryMB: 400})
	if err != nil {
		t.Fatalf("assign b-0: %v", err)
	}
	if a.NodeID != b.NodeID {
		t.Fatalf("best-fit should consolidate onto one node, got %s and %s", a.NodeID, b.NodeID)
	}
}

func TestAntiAffinityBeatsPacking(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 2, MemoryMB: 2000})
	reg.Register(types.Node{ID: "n2", CPU: 2, MemoryMB: 2000})
	s := New(reg)

	// Two replicas of the *same* workload: anti-affinity must spread them
	// across nodes even though best-fit alone would consolidate.
	a0, err := s.Assign("w-0", "w", types.Resources{MemoryMB: 400})
	if err != nil {
		t.Fatalf("assign w-0: %v", err)
	}
	a1, err := s.Assign("w-1", "w", types.Resources{MemoryMB: 400})
	if err != nil {
		t.Fatalf("assign w-1: %v", err)
	}
	if a0.NodeID == a1.NodeID {
		t.Fatalf("siblings should spread (anti-affinity primary), both on %s", a0.NodeID)
	}
}

func TestFreeingCapacitySchedulesPending(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 1, MemoryMB: 1000})
	s := New(reg)

	if _, err := s.Assign("a-0", "a", types.Resources{MemoryMB: 600}); err != nil {
		t.Fatalf("600MB should fit: %v", err)
	}
	// 600 used, 250 free of 850 → 600 more doesn't fit.
	if _, err := s.Assign("b-0", "b", types.Resources{MemoryMB: 600}); err == nil {
		t.Fatal("expected no-fit while 600MB is allocated")
	}
	// Free the first; now the second fits.
	s.Unassign("a-0")
	if _, err := s.Assign("b-0", "b", types.Resources{MemoryMB: 600}); err != nil {
		t.Fatalf("b-0 should fit after freeing a-0: %v", err)
	}
}
