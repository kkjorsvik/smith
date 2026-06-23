package reconciler

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestSpecHashChangesOnContainerFields(t *testing.T) {
	base := types.Workload{
		ID:    "nginx",
		Image: "nginx:1.27",
		Args:  []string{"-g", "daemon off;"},
		Env:   map[string]string{"A": "1", "B": "2"},
		Ports: []types.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
	}
	h := specHash(base)

	// Stable for the same spec.
	if specHash(base) != h {
		t.Fatal("specHash not deterministic for identical workloads")
	}

	// Env key order must not matter (built from a different literal order).
	reordered := base
	reordered.Env = map[string]string{"B": "2", "A": "1"}
	if specHash(reordered) != h {
		t.Fatal("specHash changed with Env key order")
	}

	// Each container-defining change must change the hash.
	changes := map[string]func(*types.Workload){
		"image":     func(w *types.Workload) { w.Image = "nginx:1.28" },
		"args":      func(w *types.Workload) { w.Args = []string{"-g", "daemon on;"} },
		"env":       func(w *types.Workload) { w.Env = map[string]string{"A": "1", "B": "3"} },
		"ports":     func(w *types.Workload) { w.Ports = []types.PortMapping{{HostPort: 9090, ContainerPort: 80}} },
		"resources": func(w *types.Workload) { w.Resources = &types.Resources{MemoryMB: 256} },
	}
	for name, mutate := range changes {
		w := base
		mutate(&w)
		if specHash(w) == h {
			t.Fatalf("specHash unchanged after %s change", name)
		}
	}
}

func TestSpecHashIgnoresScalingAndRolloutFields(t *testing.T) {
	base := types.Workload{ID: "nginx", Image: "nginx:1.27", Replicas: 1}
	h := specHash(base)

	scaled := base
	scaled.Replicas = 5
	if specHash(scaled) != h {
		t.Fatal("changing Replicas must not change specHash (scaling is not a roll)")
	}

	budget := base
	budget.MaxUnavailable = 3
	if specHash(budget) != h {
		t.Fatal("changing MaxUnavailable must not change specHash")
	}
}

func TestMaxUnavailableDefault(t *testing.T) {
	if got := maxUnavailable(types.Workload{}); got != 1 {
		t.Fatalf("default MaxUnavailable = %d, want 1", got)
	}
	if got := maxUnavailable(types.Workload{MaxUnavailable: 2}); got != 2 {
		t.Fatalf("MaxUnavailable = %d, want 2", got)
	}
}
