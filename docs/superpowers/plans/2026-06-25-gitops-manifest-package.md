# GitOps Manifest Package Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `internal/manifest` package that parses a GitOps app-bundle YAML file and lowers it into the concrete `types.Workload`/`Service`/`Ingress` resources the control plane already accepts.

**Architecture:** A pure, dependency-light package with two public entry points — `Parse([]byte) (*App, error)` and `(*App) Resolve() (*Resolved, error)`. `Parse` decodes YAML strictly (unknown keys rejected). `Resolve` applies defaults, enforces the schema's validation rules, fills in implicit names/cross-references, and returns concrete `types.*` values. No HTTP, no filesystem walking, no SOPS — those belong to the downstream apply-engine plan. This package is the approved manifest-schema spec made real and is fully unit-testable in isolation.

**Tech Stack:** Go 1.26, `sigs.k8s.io/yaml` (YAML→JSON bridge so the existing `json:` struct tags and `types.Duration`'s custom JSON marshaling are honored without duplicating tags), standard `regexp`/`fmt`, the existing `internal/types` package.

**Spec:** `docs/superpowers/specs/2026-06-25-gitops-manifest-schema-design.md`

---

## File Structure

- `internal/manifest/manifest.go` — bundle types (`App`, `WorkloadSpec`, `ServiceSpec`, `IngressSpec`) and `Parse`. Responsibility: the on-disk format and decoding it.
- `internal/manifest/resolve.go` — `Resolved` type, `Resolve`, and the private `resolveWorkload`/`resolveServices`/`resolveIngresses` helpers. Responsibility: defaults, validation, and lowering to `types.*`.
- `internal/manifest/manifest_test.go` — `Parse` tests.
- `internal/manifest/resolve_test.go` — `Resolve` happy-path, list-mode, and validation-error tests.

Why two files: parsing (format) and resolution (semantics) are distinct responsibilities that change for different reasons. Tests mirror the split.

---

## Task 1: Add the `sigs.k8s.io/yaml` dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the module**

Run:
```bash
cd /home/kkjorsvik/Projects/smith
go get sigs.k8s.io/yaml@v1.4.0
```
Expected: `go.mod` gains a `sigs.k8s.io/yaml v1.4.0` require line; `go.sum` updated. (This module wraps the `gopkg.in/yaml.v3` already present transitively and adds the YAML→JSON bridge.)

- [ ] **Step 2: Verify it resolves and the tree still builds**

Run:
```bash
go build ./... && grep sigs.k8s.io/yaml go.mod
```
Expected: build succeeds; grep prints the `sigs.k8s.io/yaml v1.4.0` line.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "build: add sigs.k8s.io/yaml for manifest parsing"
```

---

## Task 2: Bundle types and `Parse`

**Files:**
- Create: `internal/manifest/manifest.go`
- Test: `internal/manifest/manifest_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/manifest/manifest_test.go`:
```go
package manifest

import "testing"

func TestParseMinimalBundle(t *testing.T) {
	data := []byte(`
name: deployops
workload:
  image: git.kkjorsvik.com/kydovik/deployops:2026.06.24
  replicas: 2
service:
  port: 8080
ingress:
  host: deployops.kkjorsvik.com
`)
	app, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if app.Name != "deployops" {
		t.Errorf("Name = %q, want deployops", app.Name)
	}
	if app.Workload.Image != "git.kkjorsvik.com/kydovik/deployops:2026.06.24" {
		t.Errorf("Image = %q", app.Workload.Image)
	}
	if app.Workload.Replicas != 2 {
		t.Errorf("Replicas = %d, want 2", app.Workload.Replicas)
	}
	if app.Service == nil || app.Service.Port != 8080 {
		t.Errorf("Service = %+v, want port 8080", app.Service)
	}
	if app.Ingress == nil || app.Ingress.Host != "deployops.kkjorsvik.com" {
		t.Errorf("Ingress = %+v", app.Ingress)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	data := []byte(`
name: deployops
workload:
  image: nginx
  replicaz: 2
`)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected error for unknown field replicaz, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/manifest/ -run TestParse -v`
Expected: compile failure / FAIL — `undefined: Parse`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/manifest/manifest.go`:
```go
// Package manifest defines the GitOps app-bundle file format and lowers a
// parsed bundle into the concrete types.* resources the control plane accepts.
package manifest

import (
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/kkjorsvik/smith/internal/types"
)

// App is one application bundle: exactly one workload plus optional service(s)
// and ingress(es). It is the on-disk GitOps manifest format. Field names use
// json tags so sigs.k8s.io/yaml (which converts YAML to JSON) honors the same
// snake_case keys as the HTTP API and reuses types.* sub-structs verbatim,
// including types.Duration's custom JSON (un)marshaling.
type App struct {
	Name      string        `json:"name"`
	Workload  WorkloadSpec  `json:"workload"`
	Service   *ServiceSpec  `json:"service,omitempty"`
	Services  []ServiceSpec `json:"services,omitempty"`
	Ingress   *IngressSpec  `json:"ingress,omitempty"`
	Ingresses []IngressSpec `json:"ingresses,omitempty"`
}

// WorkloadSpec is the workload section. It omits Workload.ID (implicit: the
// app Name) and carries only fields a manifest author sets.
type WorkloadSpec struct {
	Image          string              `json:"image"`
	Args           []string            `json:"args,omitempty"`
	Replicas       int                 `json:"replicas,omitempty"`
	MaxUnavailable int                 `json:"max_unavailable,omitempty"`
	Env            map[string]string   `json:"env,omitempty"`
	Resources      *types.Resources    `json:"resources,omitempty"`
	Volumes        []types.Volume      `json:"volumes,omitempty"`
	Ports          []types.PortMapping `json:"ports,omitempty"`
	HealthCheck    *types.HealthCheck  `json:"health_check,omitempty"`
}

// ServiceSpec is a service section. It omits WorkloadID (implicit: the app
// Name) and the control-plane-assigned ClusterIP. NodePort may be set only as
// an explicit pin.
type ServiceSpec struct {
	Name       string `json:"name,omitempty"`
	Port       int    `json:"port"`
	TargetPort int    `json:"target_port,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	NodePort   int    `json:"node_port,omitempty"`
}

// IngressSpec is an ingress section. Service may be empty in singular mode
// (defaults to the app's sole service).
type IngressSpec struct {
	Host    string `json:"host"`
	Service string `json:"service,omitempty"`
}

// Parse decodes one YAML app bundle. Unknown fields are rejected so typos in
// keys fail loudly instead of being silently dropped.
func Parse(data []byte) (*App, error) {
	var app App
	if err := yaml.UnmarshalStrict(data, &app); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &app, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/manifest/ -run TestParse -v`
Expected: PASS (both `TestParseMinimalBundle` and `TestParseRejectsUnknownField`).

- [ ] **Step 5: Commit**

```bash
git add internal/manifest/manifest.go internal/manifest/manifest_test.go
git commit -m "feat(manifest): app-bundle types and strict Parse"
```

---

## Task 3: `Resolve` — singular happy path with defaults

**Files:**
- Create: `internal/manifest/resolve.go`
- Test: `internal/manifest/resolve_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/manifest/resolve_test.go`:
```go
package manifest

import "testing"

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

	// Workload: implicit ID, defaulted replicas/max_unavailable.
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

	// Service: implicit name = app name, workload_id = app name, target=port, tcp.
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

	// Ingress: implicit service ref = the sole service.
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/manifest/ -run TestResolve -v`
Expected: compile failure / FAIL — `app.Resolve undefined`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/manifest/resolve.go`:
```go
package manifest

import (
	"fmt"
	"regexp"

	"github.com/kkjorsvik/smith/internal/types"
)

// nameRe is the identifier pattern shared with the HTTP API for workload and
// service names.
var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Resolved is an App lowered into the concrete API resources the control plane
// accepts: exactly one workload, plus zero or more services and ingresses with
// implicit names, defaults, and cross-references filled in.
type Resolved struct {
	Workload  types.Workload
	Services  []types.Service
	Ingresses []types.Ingress
}

// Resolve validates the bundle and lowers it to concrete types.* values.
func (a *App) Resolve() (*Resolved, error) {
	if a.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if !nameRe.MatchString(a.Name) {
		return nil, fmt.Errorf("name %q must match [a-z0-9-]+", a.Name)
	}
	if a.Service != nil && len(a.Services) > 0 {
		return nil, fmt.Errorf("set either service or services, not both")
	}
	if a.Ingress != nil && len(a.Ingresses) > 0 {
		return nil, fmt.Errorf("set either ingress or ingresses, not both")
	}

	wl, err := a.resolveWorkload()
	if err != nil {
		return nil, err
	}
	svcs, err := a.resolveServices()
	if err != nil {
		return nil, err
	}
	ings, err := a.resolveIngresses(svcs)
	if err != nil {
		return nil, err
	}
	return &Resolved{Workload: wl, Services: svcs, Ingresses: ings}, nil
}

func (a *App) resolveWorkload() (types.Workload, error) {
	w := a.Workload
	if w.Image == "" {
		return types.Workload{}, fmt.Errorf("workload.image is required")
	}
	replicas := w.Replicas
	if replicas == 0 {
		replicas = 1
	}
	if len(w.Volumes) > 0 && replicas > 1 {
		return types.Workload{}, fmt.Errorf("workload with volumes must have replicas: 1 (single writer)")
	}
	maxUnavail := w.MaxUnavailable
	if maxUnavail == 0 {
		maxUnavail = 1
	}
	return types.Workload{
		ID:             a.Name,
		Image:          w.Image,
		Args:           w.Args,
		Env:            w.Env,
		Resources:      w.Resources,
		Replicas:       replicas,
		MaxUnavailable: maxUnavail,
		Volumes:        w.Volumes,
		Ports:          w.Ports,
		HealthCheck:    w.HealthCheck,
	}, nil
}

func (a *App) resolveServices() ([]types.Service, error) {
	var specs []ServiceSpec
	singular := false
	if a.Service != nil {
		specs = []ServiceSpec{*a.Service}
		singular = true
	} else {
		specs = a.Services
	}

	seen := map[string]bool{}
	var out []types.Service
	for i, s := range specs {
		name := s.Name
		if name == "" {
			if singular {
				name = a.Name
			} else {
				return nil, fmt.Errorf("services[%d].name is required in list mode", i)
			}
		}
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("service name %q must match [a-z0-9-]+", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate service name %q", name)
		}
		seen[name] = true
		if s.Port <= 0 {
			return nil, fmt.Errorf("service %q: port is required", name)
		}
		target := s.TargetPort
		if target == 0 {
			target = s.Port
		}
		proto := s.Protocol
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, types.Service{
			Name:       name,
			WorkloadID: a.Name,
			Port:       s.Port,
			TargetPort: target,
			Protocol:   proto,
			NodePort:   s.NodePort,
		})
	}
	return out, nil
}

func (a *App) resolveIngresses(svcs []types.Service) ([]types.Ingress, error) {
	var specs []IngressSpec
	if a.Ingress != nil {
		specs = []IngressSpec{*a.Ingress}
	} else {
		specs = a.Ingresses
	}
	if len(specs) == 0 {
		return nil, nil
	}

	svcNames := map[string]bool{}
	for _, s := range svcs {
		svcNames[s.Name] = true
	}

	var out []types.Ingress
	for i, in := range specs {
		if in.Host == "" {
			return nil, fmt.Errorf("ingresses[%d].host is required", i)
		}
		svc := in.Service
		if svc == "" {
			if len(svcs) != 1 {
				return nil, fmt.Errorf("ingress %q: service is required unless the app declares exactly one service (has %d)", in.Host, len(svcs))
			}
			svc = svcs[0].Name
		} else if !svcNames[svc] {
			return nil, fmt.Errorf("ingress %q: service %q is not declared in this app", in.Host, svc)
		}
		out = append(out, types.Ingress{Host: in.Host, Service: svc})
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/manifest/ -run TestResolve -v`
Expected: PASS (`TestResolveSingularDefaults`, `TestResolveNoServiceNoIngress`).

- [ ] **Step 5: Commit**

```bash
git add internal/manifest/resolve.go internal/manifest/resolve_test.go
git commit -m "feat(manifest): Resolve singular bundles with defaults and implicit naming"
```

---

## Task 4: `Resolve` — list mode (multiple services, explicit ingress ref)

**Files:**
- Modify: `internal/manifest/resolve_test.go` (add a test; implementation already supports lists)

- [ ] **Step 1: Update the test file imports**

The list-mode test references `types.Volume`. Change the import block at the top of `internal/manifest/resolve_test.go` (currently just `import "testing"`) to:
```go
import (
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)
```

- [ ] **Step 2: Write the failing test**

Append to `internal/manifest/resolve_test.go`:
```go
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
	// Both services back the single workload.
	for _, s := range got.Services {
		if s.WorkloadID != "gitea" {
			t.Errorf("service %q WorkloadID = %q, want gitea", s.Name, s.WorkloadID)
		}
	}
	if len(got.Ingresses) != 1 || got.Ingresses[0].Service != "gitea-http" {
		t.Errorf("ingress = %+v, want service gitea-http", got.Ingresses)
	}
}
```

- [ ] **Step 3: Run test to verify it fails or passes**

Run: `go test ./internal/manifest/ -run TestResolveListMode -v`
Expected: PASS (the Task 3 implementation already handles lists). This task is a coverage lock-in for list mode; if it fails, the failure message points at the exact resolved field that is wrong — fix `resolve.go` accordingly.

- [ ] **Step 4: Commit**

```bash
git add internal/manifest/resolve_test.go
git commit -m "test(manifest): cover list-mode services and explicit ingress ref"
```

---

## Task 5: `Resolve` — validation errors

**Files:**
- Modify: `internal/manifest/resolve_test.go` (add a table-driven error test)

- [ ] **Step 1: Write the failing test**

Append to `internal/manifest/resolve_test.go`:
```go
func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name string
		app  *App
		want string // substring expected in the error
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
```

Add `"strings"` to the test file's import block so it reads:
```go
import (
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./internal/manifest/ -run TestResolveErrors -v`
Expected: PASS for every sub-case. The Task 3 implementation already produces these errors; if any sub-case fails, the message shows the actual vs. expected substring — adjust the corresponding check in `resolve.go` so its error text contains the expected substring.

- [ ] **Step 3: Commit**

```bash
git add internal/manifest/resolve_test.go
git commit -m "test(manifest): cover Resolve validation errors"
```

---

## Task 6: Full-package green + vet

**Files:** none (verification only)

- [ ] **Step 1: Run the whole package test suite**

Run: `go test ./internal/manifest/ -v`
Expected: PASS — all of `TestParse*`, `TestResolve*`.

- [ ] **Step 2: Vet and full build**

Run: `go vet ./internal/manifest/ && go build ./...`
Expected: no output from vet, build succeeds.

- [ ] **Step 3: Commit (only if vet/build prompted a fix; otherwise skip)**

```bash
git commit -am "chore(manifest): vet/build clean"
```

---

## Self-Review Notes (author)

Spec coverage check against `2026-06-25-gitops-manifest-schema-design.md`:

- App-bundle shape, singular + list escape hatch → Tasks 2–4.
- snake_case fields matching json tags, reuse of `types.*` sub-structs incl. `Duration` → Task 1 (`sigs.k8s.io/yaml`) + Task 2 types.
- Implicit naming (`workload.id`/`service.name`/`service.workload_id`/`ingress.service`) → Task 3 + Task 4.
- Defaults (replicas 1, max_unavailable 1, protocol tcp, target_port=port) → Task 3 test + impl.
- Mutual exclusivity, volumes⇒replicas 1, count-based ingress rule, duplicate/unknown/missing checks → Task 5.
- `node_port` pin passes through; `cluster_ip` structurally unauthorable (no field) → Task 2 types (verified by absence).

Out of scope by design (separate downstream plans): directory walking, the SOPS `.sops.yaml` overlay merge, HTTP POST to the API, and pruning. The package deliberately stops at `Resolve` returning `types.*` values.

**Next plan (not this one):** the apply engine — walk `apps/`, `Parse`+`Resolve` each bundle, and POST to `/workloads`/`/services`/`/ingresses`. That plan needs a short brainstorm first on one open question: whether `POST /workloads` (and services/ingresses) is upsert/idempotent or needs create-vs-update handling for re-apply.
