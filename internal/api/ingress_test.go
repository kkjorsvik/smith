package api

import (
	"path/filepath"
	"testing"

	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/types"
)

func TestComputeIngressRules(t *testing.T) {
	dir := t.TempDir()

	svcStore, err := reconciler.NewServiceStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewServiceStore: %v", err)
	}
	defer svcStore.Close()
	forgejo, err := svcStore.Add(types.Service{Name: "forgejo", WorkloadID: "forgejo", Port: 3000, TargetPort: 3000})
	if err != nil {
		t.Fatalf("add service: %v", err)
	}

	ingStore, err := reconciler.NewIngressStore(filepath.Join(dir, "ing.db"))
	if err != nil {
		t.Fatalf("NewIngressStore: %v", err)
	}
	defer ingStore.Close()
	if err := ingStore.Add(types.Ingress{Host: "git.kkjorsvik.com", Service: "forgejo"}); err != nil {
		t.Fatalf("add ingress: %v", err)
	}
	// An ingress whose service doesn't exist must be skipped.
	if err := ingStore.Add(types.Ingress{Host: "ci.kkjorsvik.com", Service: "woodpecker"}); err != nil {
		t.Fatalf("add ingress: %v", err)
	}

	s := &Server{services: svcStore, ingresses: ingStore}
	rules, err := s.computeIngressRules()
	if err != nil {
		t.Fatalf("computeIngressRules: %v", err)
	}

	if len(rules) != 1 {
		t.Fatalf("expected 1 resolved rule (woodpecker skipped), got %d: %+v", len(rules), rules)
	}
	r := rules[0]
	if r.Host != "git.kkjorsvik.com" || r.ClusterIP != forgejo.ClusterIP || r.Port != 3000 {
		t.Fatalf("rule = %+v, want git.kkjorsvik.com -> %s:3000", r, forgejo.ClusterIP)
	}
}
