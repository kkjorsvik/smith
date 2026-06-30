package scheduler

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// Among nodes that tie on anti-affinity and best-fit, placement must be
// deterministic (lowest node ID), not dependent on map-iteration order — so a
// rebalance simulation is reproducible and idempotent. Fresh schedulers are
// used each iteration to re-roll Go's map ordering.
func TestAssignTiebreakIsDeterministic(t *testing.T) {
	for i := 0; i < 50; i++ {
		reg := registry.New()
		for _, id := range []string{"n2", "n3", "n1"} {
			reg.Register(types.Node{ID: id, CPU: 4, MemoryMB: 4000})
		}
		s := New(reg)
		a, err := s.Assign("app-0", "app", types.Resources{MemoryMB: 500})
		if err != nil {
			t.Fatalf("assign: %v", err)
		}
		if a.NodeID != "n1" {
			t.Fatalf("iteration %d: tied placement landed on %s, want deterministic n1", i, a.NodeID)
		}
	}
}
