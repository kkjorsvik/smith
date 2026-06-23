package api

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/containerd/containerd"
	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/scheduler"
	"github.com/kkjorsvik/smith/internal/types"
)

func TestComputeEndpoints(t *testing.T) {
	svcStore, err := reconciler.NewServiceStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewServiceStore: %v", err)
	}
	defer svcStore.Close()
	if _, err := svcStore.Add(types.Service{Name: "web", WorkloadID: "nginx", Port: 80, TargetPort: 80}); err != nil {
		t.Fatalf("add service: %v", err)
	}

	reg := registry.New()
	reg.Register(types.Node{ID: "n1", Addr: "n1:9000"})
	reg.Register(types.Node{ID: "n2", Addr: "n2:9000"})
	sched := scheduler.New(reg)

	a0, _ := sched.Assign("nginx-0", "nginx")
	a1, _ := sched.Assign("nginx-1", "nginx")
	a2, _ := sched.Assign("nginx-2", "nginx")

	// nginx-0 and nginx-1 are running with IPs; nginx-2 is not running (must
	// be excluded). Keyed by the nodes the scheduler actually chose.
	status := map[string]map[string]runtime.ContainerStatus{}
	put := func(a types.Assignment, st, ip string) {
		if status[a.NodeID] == nil {
			status[a.NodeID] = map[string]runtime.ContainerStatus{}
		}
		status[a.NodeID][a.WorkloadID] = runtime.ContainerStatus{ID: a.WorkloadID, Status: containerd.ProcessStatus(st), IP: ip}
	}
	put(a0, "running", "10.22.0.5")
	put(a1, "running", "10.22.1.5")
	put(a2, "stopped", "") // excluded: not running, no IP

	s := &Server{
		services:   svcStore,
		scheduler:  sched,
		statusFunc: func() map[string]map[string]runtime.ContainerStatus { return status },
	}

	eps := s.computeEndpoints()
	if len(eps) != 1 {
		t.Fatalf("expected 1 service, got %d", len(eps))
	}
	se := eps[0]
	if se.ClusterIP == "" || se.NodePort == 0 {
		t.Fatalf("service missing ClusterIP/NodePort: %+v", se)
	}

	got := append([]string(nil), se.Endpoints...)
	sort.Strings(got)
	want := []string{"10.22.0.5", "10.22.1.5"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("endpoints = %v, want %v (nginx-2 excluded)", se.Endpoints, want)
	}
}
