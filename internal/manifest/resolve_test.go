package manifest

import (
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestResolveSingularDefaults(t *testing.T) {
	app := &App{
		Name:     "deployops",
		Workload: WorkloadSpec{Image: "nginx", Env: map[string]string{"LOG_LEVEL": "info"}},
		Service:  &ServiceSpec{Port: 8080},
		Ingress:  &IngressSpec{Host: "deployops.kkjorsvik.com"},
	}
	got, err := app.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Workload.ID != "deployops" {
		t.Errorf("Workload.ID = %q, want deployops", got.Workload.ID)
	}
	if got.Workload.Replicas != 1 {
		t.Errorf("Replicas = %d, want default 1", got.Workload.Replicas)
	}
	if got.Workload.MaxUnavailable != 1 {
		t.Errorf("MaxUnavailable = %d, want default 1", got.Workload.MaxUnavailable)
	}
	if got.Workload.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Env not carried through: %+v", got.Workload.Env)
	}
	if len(got.Services) != 1 {
		t.Fatalf("len(Services) = %d, want 1", len(got.Services))
	}
	svc := got.Services[0]
	if svc.Name != "deployops" || svc.WorkloadID != "deployops" {
		t.Errorf("svc name/workload = %q/%q, want deployops/deployops", svc.Name, svc.WorkloadID)
	}
	if svc.Port != 8080 || svc.TargetPort != 8080 {
		t.Errorf("port/target = %d/%d, want 8080/8080", svc.Port, svc.TargetPort)
	}
	if svc.Protocol != "tcp" {
		t.Errorf("Protocol = %q, want tcp", svc.Protocol)
	}
	if len(got.Ingresses) != 1 {
		t.Fatalf("len(Ingresses) = %d, want 1", len(got.Ingresses))
	}
	if got.Ingresses[0].Service != "deployops" || got.Ingresses[0].Host != "deployops.kkjorsvik.com" {
		t.Errorf("ingress = %+v", got.Ingresses[0])
	}
}

func TestResolveNoServiceNoIngress(t *testing.T) {
	app := &App{
		Name:     "batch",
		Workload: WorkloadSpec{Image: "busybox"},
	}
	got, err := app.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Services) != 0 || len(got.Ingresses) != 0 {
		t.Errorf("want no services/ingresses, got %d/%d", len(got.Services), len(got.Ingresses))
	}
	if got.Workload.ID != "batch" {
		t.Errorf("Workload.ID = %q, want batch", got.Workload.ID)
	}
}

func TestResolveListMode(t *testing.T) {
	app := &App{
		Name:     "gitea",
		Workload: WorkloadSpec{Image: "gitea:1.22", Volumes: []types.Volume{{Name: "data", Path: "/data"}}},
		Services: []ServiceSpec{
			{Name: "gitea-http", Port: 3000},
			{Name: "gitea-ssh", Port: 22},
		},
		Ingresses: []IngressSpec{
			{Host: "git.kkjorsvik.com", Service: "gitea-http"},
		},
	}
	got, err := app.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("len(Services) = %d, want 2", len(got.Services))
	}
	if got.Services[0].Name != "gitea-http" || got.Services[1].Name != "gitea-ssh" {
		t.Errorf("service names = %q, %q", got.Services[0].Name, got.Services[1].Name)
	}
	for _, s := range got.Services {
		if s.WorkloadID != "gitea" {
			t.Errorf("service %q WorkloadID = %q, want gitea", s.Name, s.WorkloadID)
		}
	}
	if len(got.Ingresses) != 1 || got.Ingresses[0].Service != "gitea-http" {
		t.Errorf("ingress = %+v, want service gitea-http", got.Ingresses)
	}
}

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name string
		app  *App
		want string
	}{
		{
			name: "missing name",
			app:  &App{Workload: WorkloadSpec{Image: "nginx"}},
			want: "name is required",
		},
		{
			name: "bad name chars",
			app:  &App{Name: "Deploy_Ops", Workload: WorkloadSpec{Image: "nginx"}},
			want: "must match",
		},
		{
			name: "missing image",
			app:  &App{Name: "app", Workload: WorkloadSpec{}},
			want: "image is required",
		},
		{
			name: "service and services both set",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Service:  &ServiceSpec{Port: 80},
				Services: []ServiceSpec{{Name: "x", Port: 81}},
			},
			want: "either service or services",
		},
		{
			name: "ingress and ingresses both set",
			app: &App{
				Name:      "app",
				Workload:  WorkloadSpec{Image: "nginx"},
				Service:   &ServiceSpec{Port: 80},
				Ingress:   &IngressSpec{Host: "a.example.com"},
				Ingresses: []IngressSpec{{Host: "b.example.com"}},
			},
			want: "either ingress or ingresses",
		},
		{
			name: "volumes force single replica",
			app: &App{
				Name: "db",
				Workload: WorkloadSpec{
					Image:    "postgres:18",
					Replicas: 2,
					Volumes:  []types.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}},
				},
			},
			want: "replicas: 1",
		},
		{
			name: "list service missing name",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Services: []ServiceSpec{{Port: 80}},
			},
			want: "name is required in list mode",
		},
		{
			name: "service missing port",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Service:  &ServiceSpec{},
			},
			want: "port is required",
		},
		{
			name: "duplicate service names",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Services: []ServiceSpec{{Name: "dup", Port: 80}, {Name: "dup", Port: 81}},
			},
			want: "duplicate service name",
		},
		{
			name: "ingress with no service",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Ingress:  &IngressSpec{Host: "a.example.com"},
			},
			want: "service is required",
		},
		{
			name: "ingress with two services and no ref",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Services: []ServiceSpec{{Name: "a", Port: 80}, {Name: "b", Port: 81}},
				Ingress:  &IngressSpec{Host: "a.example.com"},
			},
			want: "service is required",
		},
		{
			name: "ingress ref to unknown service",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Service:  &ServiceSpec{Name: "real", Port: 80},
				Ingress:  &IngressSpec{Host: "a.example.com", Service: "ghost"},
			},
			want: "not declared in this app",
		},
		{
			name: "ingress missing host",
			app: &App{
				Name:     "app",
				Workload: WorkloadSpec{Image: "nginx"},
				Service:  &ServiceSpec{Port: 80},
				Ingress:  &IngressSpec{},
			},
			want: "host is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.app.Resolve()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
