package reconciler

import (
	"path/filepath"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestServiceStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	st, err := NewServiceStore(dbPath)
	if err != nil {
		t.Fatalf("NewServiceStore: %v", err)
	}

	// First service gets the lowest ClusterIP and NodePort.
	a, err := st.Add(types.Service{Name: "web", WorkloadID: "nginx", Port: 80, TargetPort: 80})
	if err != nil {
		t.Fatalf("add web: %v", err)
	}
	if a.ClusterIP != "10.23.0.1" || a.NodePort != nodePortMin {
		t.Fatalf("web got %s:%d / nodeport %d, want 10.23.0.1 / %d", a.ClusterIP, a.Port, a.NodePort, nodePortMin)
	}
	if a.Protocol != "tcp" {
		t.Fatalf("protocol defaulted to %q, want tcp", a.Protocol)
	}

	// Second service gets the next free IP/port.
	b, err := st.Add(types.Service{Name: "api", WorkloadID: "apisrv", Port: 8080, TargetPort: 3000})
	if err != nil {
		t.Fatalf("add api: %v", err)
	}
	if b.ClusterIP != "10.23.0.2" || b.NodePort != nodePortMin+1 {
		t.Fatalf("api got %s / nodeport %d, want 10.23.0.2 / %d", b.ClusterIP, b.NodePort, nodePortMin+1)
	}

	// Update is idempotent on the allocation: same name keeps its IP/port,
	// other fields change.
	a2, err := st.Add(types.Service{Name: "web", WorkloadID: "nginx", Port: 80, TargetPort: 8080})
	if err != nil {
		t.Fatalf("update web: %v", err)
	}
	if a2.ClusterIP != a.ClusterIP || a2.NodePort != a.NodePort {
		t.Fatalf("update reallocated: %s/%d vs %s/%d", a2.ClusterIP, a2.NodePort, a.ClusterIP, a.NodePort)
	}
	if a2.TargetPort != 8080 {
		t.Fatalf("update didn't apply: target_port = %d", a2.TargetPort)
	}

	// Validation.
	if _, err := st.Add(types.Service{Name: "bad", Port: 80, TargetPort: 80}); err == nil {
		t.Fatal("expected error for missing workload_id")
	}

	// Remove frees the allocation; the next service reuses the lowest free.
	if err := st.Remove("web"); err != nil {
		t.Fatalf("remove web: %v", err)
	}
	c, err := st.Add(types.Service{Name: "cache", WorkloadID: "redis", Port: 6379, TargetPort: 6379})
	if err != nil {
		t.Fatalf("add cache: %v", err)
	}
	if c.ClusterIP != "10.23.0.1" || c.NodePort != nodePortMin {
		t.Fatalf("cache got %s/%d, want reused 10.23.0.1/%d", c.ClusterIP, c.NodePort, nodePortMin)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Persistence: reopen and confirm allocations survive.
	st2, err := NewServiceStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	again, err := st2.Add(types.Service{Name: "api", WorkloadID: "apisrv", Port: 8080, TargetPort: 3000})
	if err != nil {
		t.Fatalf("re-add api after reopen: %v", err)
	}
	if again.ClusterIP != b.ClusterIP || again.NodePort != b.NodePort {
		t.Fatalf("api allocation changed after reopen: %s/%d vs %s/%d", again.ClusterIP, again.NodePort, b.ClusterIP, b.NodePort)
	}
}
