# GitOps Apply Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `smithctl apply <dir>` — a dedicated operator CLI that resolves a directory of GitOps app bundles (via `internal/manifest`) and POSTs the resulting resources to the control-plane API.

**Architecture:** Three small packages. `internal/client` is a pure-HTTP API client plus a kubeconfig-style config loader. `internal/apply` orchestrates: it validates every bundle (parse+resolve) before mutating anything, then applies in dependency order (workloads → services → ingresses) through a small `Cluster` interface (real client in prod, fake in tests). `cmd/smithctl` is a thin CLI wiring config + client + apply. No containerd/CNI/netlink dependencies — the binary builds anywhere.

**Tech Stack:** Go 1.26, `net/http`, `net/http/httptest` (tests), `sigs.k8s.io/yaml` (config parsing — already a dependency), the existing `internal/manifest` and `internal/types` packages.

**Spec:** `docs/superpowers/specs/2026-06-25-gitops-apply-engine-design.md`

---

## File Structure

- `internal/client/config.go` — `Config{Server,Token}`, `DefaultConfigPath()`, `LoadConfig(path)`. Responsibility: where connection settings come from.
- `internal/client/client.go` — `Client` + `New(Config)` + `ApplyWorkload/ApplyService/ApplyIngress`. Responsibility: talking HTTP to the API.
- `internal/apply/apply.go` — `Cluster` interface + `Apply(dir, cluster, dryRun, out)` + private `manifestFiles`/`printPlan`. Responsibility: orchestration and safety (validate-all, ordering, dry-run).
- `cmd/smithctl/main.go` — CLI entry, flag parsing, wiring. No business logic.
- Tests: `internal/client/config_test.go`, `internal/client/client_test.go`, `internal/apply/apply_test.go`.

---

## Task 1: Config loader (`internal/client/config.go`)

**Files:**
- Create: `internal/client/config.go`
- Test: `internal/client/config_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/client/config_test.go`:
```go
package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	p := writeConfig(t, "server: https://h.example.com\ntoken: abc\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "https://h.example.com" || cfg.Token != "abc" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "no config at") {
		t.Fatalf("err = %v, want 'no config at'", err)
	}
}

func TestLoadConfigMissingServer(t *testing.T) {
	p := writeConfig(t, "token: abc\n")
	_, err := LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "server is required") {
		t.Fatalf("err = %v, want 'server is required'", err)
	}
}

func TestLoadConfigMissingToken(t *testing.T) {
	p := writeConfig(t, "server: https://h\n")
	_, err := LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("err = %v, want 'token is required'", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/client/ -run TestLoadConfig -v`
Expected: compile failure — `undefined: LoadConfig`.

- [ ] **Step 3: Write the implementation**

Create `internal/client/config.go`:
```go
// Package client is a pure-HTTP client for the smith control-plane API plus the
// kubeconfig-style config loader smithctl uses to find the server and token.
package client

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Config holds the connection settings for the control-plane API.
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// DefaultConfigPath returns ~/.config/smith/config.
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".config", "smith", "config"), nil
}

// LoadConfig reads and validates the YAML config at path. Both server and token
// are required.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("no config at %s; set server and token", path)
		}
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Server == "" {
		return Config{}, fmt.Errorf("config %s: server is required", path)
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("config %s: token is required", path)
	}
	return cfg, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/client/ -run TestLoadConfig -v`
Expected: PASS (all four).

- [ ] **Step 5: Commit**

```bash
git add internal/client/config.go internal/client/config_test.go
git commit -m "feat(client): kubeconfig-style config loader"
```

---

## Task 2: HTTP client (`internal/client/client.go`)

**Files:**
- Create: `internal/client/client.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/client/client_test.go`:
```go
package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestApplyWorkloadPostsWithAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody types.Workload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(gotBody)
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "tok123"})
	if err := c.ApplyWorkload(types.Workload{ID: "postgres", Image: "postgres:18"}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/workloads" {
		t.Errorf("got %s %s, want POST /workloads", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("auth = %q, want Bearer tok123", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotBody.ID != "postgres" || gotBody.Image != "postgres:18" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestApplyServiceAndIngressPaths(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	// Trailing slash on Server must be trimmed so paths are clean.
	c := New(Config{Server: srv.URL + "/", Token: "t"})
	if err := c.ApplyService(types.Service{Name: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyIngress(types.Ingress{Host: "h"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/services,/ingresses" {
		t.Errorf("paths = %v, want /services,/ingresses", paths)
	}
}

func TestApplyErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad workload"}`))
	}))
	defer srv.Close()
	c := New(Config{Server: srv.URL, Token: "t"})
	err := c.ApplyWorkload(types.Workload{ID: "x"})
	if err == nil || !strings.Contains(err.Error(), "bad workload") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want status 400 + body", err)
	}
}

func TestApplyUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer srv.Close()
	c := New(Config{Server: srv.URL, Token: "t"})
	err := c.ApplyWorkload(types.Workload{ID: "x"})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/client/ -run TestApply -v`
Expected: compile failure — `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `internal/client/client.go`:
```go
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kkjorsvik/smith/internal/types"
)

// Client posts resources to the control-plane public API using a bearer token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for the given config. Any trailing slash on the server
// URL is trimmed so path joins are clean.
func New(cfg Config) *Client {
	return &Client{
		baseURL: strings.TrimRight(cfg.Server, "/"),
		token:   cfg.Token,
		http:    http.DefaultClient,
	}
}

// ApplyWorkload upserts a workload (POST /workloads).
func (c *Client) ApplyWorkload(w types.Workload) error { return c.post("/workloads", w) }

// ApplyService upserts a service (POST /services).
func (c *Client) ApplyService(s types.Service) error { return c.post("/services", s) }

// ApplyIngress upserts an ingress (POST /ingresses).
func (c *Client) ApplyIngress(i types.Ingress) error { return c.post("/ingresses", i) }

// post marshals v to JSON and POSTs it to path with the bearer token, mapping
// any non-2xx response to an error that includes the status and body. The API
// is an idempotent upsert, so a repeat POST is a safe update.
func (c *Client) post(path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	trimmed := strings.TrimSpace(string(respBody))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (check token): %s", trimmed)
	}
	return fmt.Errorf("%s: %s: %s", path, resp.Status, trimmed)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/client/ -v`
Expected: PASS (config + client tests).

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): bearer-auth API client with Apply methods"
```

---

## Task 3: Apply orchestrator (`internal/apply/apply.go`)

**Files:**
- Create: `internal/apply/apply.go`
- Test: `internal/apply/apply_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/apply/apply_test.go`:
```go
package apply

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

// fakeCluster records the ordered sequence of apply calls and can be told to
// fail on a specific workload ID.
type fakeCluster struct {
	calls          []string
	failWorkloadID string
}

func (f *fakeCluster) ApplyWorkload(w types.Workload) error {
	f.calls = append(f.calls, "workload:"+w.ID)
	if w.ID == f.failWorkloadID {
		return fmt.Errorf("boom")
	}
	return nil
}
func (f *fakeCluster) ApplyService(s types.Service) error {
	f.calls = append(f.calls, "service:"+s.Name)
	return nil
}
func (f *fakeCluster) ApplyIngress(i types.Ingress) error {
	f.calls = append(f.calls, "ingress:"+i.Host)
	return nil
}

func writeManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const alphaYAML = `
name: alpha
workload:
  image: nginx
service:
  port: 80
ingress:
  host: alpha.example.com
`

const betaYAML = `
name: beta
workload:
  image: redis
`

func TestApplyOrdering(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	writeManifest(t, dir, "beta.yaml", betaYAML)

	f := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, f, false, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Files sort alpha,beta. All workloads, then services, then ingresses.
	want := "workload:alpha,workload:beta,service:alpha,ingress:alpha.example.com"
	if strings.Join(f.calls, ",") != want {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if !strings.Contains(out.String(), "applied workload alpha") {
		t.Errorf("out missing applied lines: %q", out.String())
	}
}

func TestApplyValidateAllAborts(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "good.yaml", betaYAML)
	writeManifest(t, dir, "bad.yaml", "name: bad\nworkload: {}\n") // missing image
	f := &fakeCluster{}
	err := Apply(dir, f, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(f.calls) != 0 {
		t.Errorf("expected zero applies on validation failure, got %v", f.calls)
	}
}

func TestApplyDryRun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "beta.yaml", betaYAML)
	f := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, f, true, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("dry-run applied resources: %v", f.calls)
	}
	if !strings.Contains(out.String(), "plan (dry run") {
		t.Errorf("out missing plan: %q", out.String())
	}
}

func TestApplyFailFast(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	writeManifest(t, dir, "beta.yaml", betaYAML)
	f := &fakeCluster{failWorkloadID: "beta"}
	err := Apply(dir, f, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "service:") || strings.HasPrefix(c, "ingress:") {
			t.Errorf("applied %q after a workload failure", c)
		}
	}
}

func TestApplySkipsSopsFiles(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "beta.yaml", betaYAML)
	writeManifest(t, dir, "beta.sops.yaml", "env:\n  SECRET: x\n")
	f := &fakeCluster{}
	if err := Apply(dir, f, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(f.calls, ",") != "workload:beta" {
		t.Errorf("calls = %v, want only workload:beta (sops skipped)", f.calls)
	}
}

func TestApplyNoManifests(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "only.sops.yaml", "env: {}\n")
	err := Apply(dir, &fakeCluster{}, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no manifests found") {
		t.Fatalf("err = %v, want 'no manifests found'", err)
	}
}

func TestApplyMissingDir(t *testing.T) {
	err := Apply(filepath.Join(t.TempDir(), "nope"), &fakeCluster{}, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "read dir") {
		t.Fatalf("err = %v, want 'read dir'", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/apply/ -v`
Expected: compile failure — `undefined: Apply`.

- [ ] **Step 3: Write the implementation**

Create `internal/apply/apply.go`:
```go
// Package apply resolves a directory of GitOps app bundles and applies the
// results to the control plane. It validates every bundle before mutating
// anything, then applies in dependency order (workloads, services, ingresses).
package apply

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kkjorsvik/smith/internal/manifest"
	"github.com/kkjorsvik/smith/internal/types"
)

// Cluster is the subset of the API client the apply engine needs. The real
// implementation is internal/client.Client; tests use a fake.
type Cluster interface {
	ApplyWorkload(types.Workload) error
	ApplyService(types.Service) error
	ApplyIngress(types.Ingress) error
}

// Apply resolves every app bundle in dir and applies the results. It validates
// ALL bundles (parse + resolve) before mutating anything: a single error aborts
// before any POST. With dryRun it prints the plan and applies nothing. On a real
// run it applies in dependency order and stops at the first error.
func Apply(dir string, cluster Cluster, dryRun bool, out io.Writer) error {
	files, err := manifestFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no manifests found in %s", dir)
	}

	// Validate-all: resolve every bundle first; abort before any apply on error.
	resolved := make([]*manifest.Resolved, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		app, err := manifest.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		res, err := app.Resolve()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		resolved = append(resolved, res)
	}

	if dryRun {
		printPlan(out, resolved)
		return nil
	}

	// Dependency order: workloads, then services, then ingresses.
	for _, r := range resolved {
		if err := cluster.ApplyWorkload(r.Workload); err != nil {
			return fmt.Errorf("workload %s: %w", r.Workload.ID, err)
		}
		fmt.Fprintf(out, "applied workload %s\n", r.Workload.ID)
	}
	for _, r := range resolved {
		for _, s := range r.Services {
			if err := cluster.ApplyService(s); err != nil {
				return fmt.Errorf("service %s: %w", s.Name, err)
			}
			fmt.Fprintf(out, "applied service %s\n", s.Name)
		}
	}
	for _, r := range resolved {
		for _, in := range r.Ingresses {
			if err := cluster.ApplyIngress(in); err != nil {
				return fmt.Errorf("ingress %s: %w", in.Host, err)
			}
			fmt.Fprintf(out, "applied ingress %s\n", in.Host)
		}
	}
	return nil
}

// manifestFiles returns the sorted *.yaml files in dir, excluding *.sops.yaml
// (secret overlays handled by a later step) and subdirectories.
func manifestFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".sops.yaml") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files, nil
}

// printPlan writes the resources that would be applied, grouped by kind.
func printPlan(out io.Writer, resolved []*manifest.Resolved) {
	fmt.Fprintln(out, "plan (dry run, nothing applied):")
	for _, r := range resolved {
		fmt.Fprintf(out, "  workload %s  image=%s replicas=%d\n", r.Workload.ID, r.Workload.Image, r.Workload.Replicas)
	}
	for _, r := range resolved {
		for _, s := range r.Services {
			fmt.Fprintf(out, "  service %s  port=%d nodePort=%d\n", s.Name, s.Port, s.NodePort)
		}
	}
	for _, r := range resolved {
		for _, in := range r.Ingresses {
			fmt.Fprintf(out, "  ingress %s -> service %s\n", in.Host, in.Service)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/apply/ -v`
Expected: PASS (all seven).

- [ ] **Step 5: Commit**

```bash
git add internal/apply/apply.go internal/apply/apply_test.go
git commit -m "feat(apply): validate-all-then-apply orchestrator with dry-run"
```

---

## Task 4: The `smithctl` CLI (`cmd/smithctl/main.go`)

**Files:**
- Create: `cmd/smithctl/main.go`

- [ ] **Step 1: Write the implementation**

Create `cmd/smithctl/main.go`:
```go
// Command smithctl is the smith operator CLI. It applies a directory of GitOps
// app bundles to the cluster via the control-plane API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kkjorsvik/smith/internal/apply"
	"github.com/kkjorsvik/smith/internal/client"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "smithctl:", err)
		os.Exit(1)
	}
}

const usage = "usage: smithctl [--config PATH] apply [--dry-run] <dir>"

func run(args []string) error {
	fs := flag.NewFlagSet("smithctl", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default ~/.config/smith/config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New(usage)
	}
	switch rest[0] {
	case "apply":
		return runApply(*configPath, rest[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", rest[0], usage)
	}
}

func runApply(configPath string, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "resolve and print the plan without applying")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: smithctl apply [--dry-run] <dir>")
	}
	dir := fs.Arg(0)

	if configPath == "" {
		p, err := client.DefaultConfigPath()
		if err != nil {
			return err
		}
		configPath = p
	}
	cfg, err := client.LoadConfig(configPath)
	if err != nil {
		return err
	}
	return apply.Apply(dir, client.New(cfg), *dryRun, os.Stdout)
}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `go build -o bin/smithctl ./cmd/smithctl && echo "smithctl built"`
Expected: prints `smithctl built`.

- [ ] **Step 3: Smoke-test the error paths (no cluster needed)**

Run:
```bash
./bin/smithctl 2>&1; echo "---"; ./bin/smithctl --config /nonexistent apply ./does-not-matter 2>&1
```
Expected: first prints the usage line and exits non-zero; second prints `smithctl: no config at /nonexistent; set server and token`.

- [ ] **Step 4: Commit**

```bash
git add cmd/smithctl/main.go
git commit -m "feat(smithctl): apply CLI wiring config + client + apply"
```

---

## Task 5: Full build, vet, and suite green

**Files:** none (verification only)

- [ ] **Step 1: Run the new packages' tests**

Run: `go test ./internal/client/ ./internal/apply/ -v`
Expected: PASS for all.

- [ ] **Step 2: Vet and full build**

Run: `go vet ./internal/client/ ./internal/apply/ ./cmd/smithctl/ && go build ./...`
Expected: no vet output; build succeeds.

- [ ] **Step 3: Full test suite**

Run: `go test ./...`
Expected: all packages pass (or `[no test files]`); nothing fails.

- [ ] **Step 4: Commit (only if vet/build required a fix; otherwise skip)**

```bash
git commit -am "chore(smithctl): vet/build clean"
```

---

## Self-Review Notes (author)

Spec coverage check against `2026-06-25-gitops-apply-engine-design.md`:

- New `cmd/smithctl` binary, pure HTTP, no containerd deps → Task 4 (imports only `internal/apply` + `internal/client`).
- Validate-all-then-apply; abort before any POST on resolve error → Task 3 `Apply` + `TestApplyValidateAllAborts`.
- Dependency ordering workloads→services→ingresses → Task 3 + `TestApplyOrdering`.
- `--dry-run` prints plan, applies nothing → Task 3 `printPlan` + `TestApplyDryRun`; CLI flag in Task 4.
- Fail-fast on first POST error → Task 3 + `TestApplyFailFast`.
- `*.sops.yaml` skipped; "no manifests found"; missing dir → Task 3 `manifestFiles` + `TestApplySkipsSopsFiles`/`TestApplyNoManifests`/`TestApplyMissingDir`.
- kubeconfig-style config, default path, `--config` override, required fields → Task 1 + Task 4.
- HTTP client: bearer header, JSON body, paths, non-2xx → status+body, 401 → unauthorized → Task 2 tests.

Deliberate, minor deviation from the spec: the spec's error table phrased the 401 message as "unauthorized + config path." The client layer does not own the config path, so it emits `unauthorized (check token)` and the CLI prepends `smithctl:`. Including the path would couple the client to CLI concerns; the message is still actionable. (Not a gap — noted so a reviewer isn't surprised.)

Out of scope by design (later specs): pruning, SOPS decrypt/merge, diff-vs-live and created/updated reporting.

**Next plan (not this one):** owner-labels + pruning (and `smithctl diff` against live state), then SOPS overlays.
