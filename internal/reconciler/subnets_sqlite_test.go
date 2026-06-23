package reconciler

import (
	"path/filepath"
	"testing"
)

func TestSubnetAllocator(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	a, err := NewSubnetAllocator(dbPath, "10.22.0.0/16")
	if err != nil {
		t.Fatalf("NewSubnetAllocator: %v", err)
	}

	// First two nodes get sequential /24 blocks.
	cfgA, err := a.Allocate("node-a")
	if err != nil {
		t.Fatalf("allocate node-a: %v", err)
	}
	if cfgA.Subnet != "10.22.0.0/24" || cfgA.Gateway != "10.22.0.1" {
		t.Fatalf("node-a got %+v, want 10.22.0.0/24 / 10.22.0.1", cfgA)
	}

	cfgB, err := a.Allocate("node-b")
	if err != nil {
		t.Fatalf("allocate node-b: %v", err)
	}
	if cfgB.Subnet != "10.22.1.0/24" {
		t.Fatalf("node-b got %s, want 10.22.1.0/24", cfgB.Subnet)
	}

	// Idempotent: re-allocating an existing node returns the same subnet.
	cfgA2, err := a.Allocate("node-a")
	if err != nil {
		t.Fatalf("re-allocate node-a: %v", err)
	}
	if cfgA2 != cfgA {
		t.Fatalf("node-a re-allocate got %+v, want %+v", cfgA2, cfgA)
	}

	// Lowest free index is reused after a release.
	if err := a.Release("node-b"); err != nil {
		t.Fatalf("release node-b: %v", err)
	}
	cfgC, err := a.Allocate("node-c")
	if err != nil {
		t.Fatalf("allocate node-c: %v", err)
	}
	if cfgC.Subnet != "10.22.1.0/24" {
		t.Fatalf("node-c got %s, want reused 10.22.1.0/24", cfgC.Subnet)
	}

	// All() reflects the current allocations.
	all, err := a.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 2 || all["node-a"] != "10.22.0.0/24" || all["node-c"] != "10.22.1.0/24" {
		t.Fatalf("All() = %+v", all)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Persistence: a fresh allocator over the same file returns the same
	// subnets — the property that makes a control-plane restart safe.
	a2, err := NewSubnetAllocator(dbPath, "10.22.0.0/16")
	if err != nil {
		t.Fatalf("reopen allocator: %v", err)
	}
	defer a2.Close()

	cfgA3, err := a2.Allocate("node-a")
	if err != nil {
		t.Fatalf("allocate node-a after reopen: %v", err)
	}
	if cfgA3 != cfgA {
		t.Fatalf("after reopen node-a got %+v, want %+v", cfgA3, cfgA)
	}
}

func TestSubnetAllocatorRejectsNon16(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	if _, err := NewSubnetAllocator(dbPath, "10.22.0.0/24"); err == nil {
		t.Fatal("expected error for non-/16 cluster CIDR, got nil")
	}
}
