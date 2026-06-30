package scheduler

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// twoNodeReg registers a small node and a big node, returning a scheduler.
// schedulable mem: small = smallMB*0.85, big = bigMB*0.85.
func twoNodeReg(smallMB, bigMB int) *Scheduler {
	reg := registry.New()
	reg.Register(types.Node{ID: "small", CPU: 4, MemoryMB: smallMB})
	reg.Register(types.Node{ID: "big", CPU: 4, MemoryMB: bigMB})
	return New(reg)
}

// Fix 2: when a placed replica's request grows beyond its current node's
// schedulable cap, the sticky path must release it and re-place it on a node
// that fits — not silently keep it and overcommit the node.
func TestStickyRefitWhenRequestOutgrowsNode(t *testing.T) {
	s := twoNodeReg(2000, 8000) // small cap=1700, big cap=6800

	first, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 1000})
	if err != nil {
		t.Fatalf("initial assign: %v", err)
	}
	if first.NodeID != "small" {
		t.Fatalf("best-fit should place 1000MB on the small node, got %s", first.NodeID)
	}

	// Grow the request to 1800MB, which no longer fits the small node (cap 1700).
	second, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 1800})
	if err != nil {
		t.Fatalf("re-assign after growth: %v", err)
	}
	if second.NodeID != "big" {
		t.Fatalf("grown request should be re-placed onto the big node, got %s", second.NodeID)
	}
}

// Fix 2 guard: a grown request that still fits the current node must NOT move
// the replica — re-fitting only kicks in when the node genuinely no longer fits,
// so we don't churn/restart containers on every spec refresh.
func TestStickyKeepsNodeWhenGrownRequestStillFits(t *testing.T) {
	s := twoNodeReg(2000, 8000) // small cap=1700

	first, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 1000})
	if err != nil {
		t.Fatalf("initial assign: %v", err)
	}

	// Grow to 1500MB, still within the small node's 1700 cap → stay put.
	second, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 1500})
	if err != nil {
		t.Fatalf("re-assign within cap: %v", err)
	}
	if second.NodeID != first.NodeID {
		t.Fatalf("request still fits; replica should stay on %s, moved to %s", first.NodeID, second.NodeID)
	}
}

// Fix 2 edge: if the grown request fits no node, the replica is released and
// left pending (error), not pinned to an overcommitted node.
func TestStickyRefitUnassignsWhenNoNodeFits(t *testing.T) {
	s := twoNodeReg(2000, 4000) // small cap=1700, big cap=3400

	if _, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 1000}); err != nil {
		t.Fatalf("initial assign: %v", err)
	}

	// Grow beyond every node's cap.
	if _, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 5000}); err == nil {
		t.Fatal("expected insufficient-capacity error for a 5000MB request")
	}
	if _, ok := s.GetAssignment("app-0"); ok {
		t.Fatal("replica should be unassigned (pending) after failing to re-fit")
	}
}

// Fix 1: Overcommitted surfaces nodes whose committed requests exceed their
// schedulable cap — e.g. a node that rejoins reporting less memory than the
// load already placed on it.
func TestOvercommittedFlagsNodeOverCap(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 4000}) // cap 3400
	s := New(reg)

	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.Assign(id+"-0", id, types.Resources{MemoryMB: 1000}); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}
	if over := s.Overcommitted(); len(over) != 0 {
		t.Fatalf("3000MB on a 3400-cap node should not be overcommitted, got %+v", over)
	}

	// Node rejoins with less RAM: cap drops to 2550, below the committed 3000.
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 3000})

	over := s.Overcommitted()
	if len(over) != 1 {
		t.Fatalf("expected 1 overcommitted node, got %d: %+v", len(over), over)
	}
	if over[0].NodeID != "n1" {
		t.Fatalf("expected n1 overcommitted, got %s", over[0].NodeID)
	}
	if over[0].MemoryMB != 3000 {
		t.Fatalf("committed memory = %d, want 3000", over[0].MemoryMB)
	}
	if over[0].CapMemoryMB != 2550 {
		t.Fatalf("cap memory = %d, want 2550", over[0].CapMemoryMB)
	}
}

func TestOvercommittedEmptyWithinCap(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 4000})
	s := New(reg)

	if _, err := s.Assign("a-0", "a", types.Resources{MemoryMB: 1000}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if over := s.Overcommitted(); len(over) != 0 {
		t.Fatalf("within-cap node should not be flagged, got %+v", over)
	}
}
