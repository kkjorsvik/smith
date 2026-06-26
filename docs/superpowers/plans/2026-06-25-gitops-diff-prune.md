# GitOps Diff & Prune Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `smithctl diff <dir>` (read-only delta vs. live cluster state) and `smithctl apply --prune <dir>` (delete live resources git no longer declares), making git the complete source of truth.

**Architecture:** `internal/client` gains GET (List) and DELETE methods, refactoring its non-2xx error mapping into a shared helper. `internal/apply`'s `Cluster` interface grows to nine methods; a shared `loadBundles` helper feeds both a new read-only `Diff` (compares resource identities, no decryption) and `Apply` (which gains a `prune` flag: applies desired first, then deletes `live − desired` in reverse-dependency order). `cmd/smithctl` adds the `diff` subcommand and `--prune` flag.

**Tech Stack:** Go 1.26, `net/http` + `net/http/httptest`, `net/url` (path-escaping delete keys), the existing `internal/apply`/`internal/client`/`internal/manifest`/`internal/types` packages.

**Spec:** `docs/superpowers/specs/2026-06-25-gitops-diff-prune-design.md`

---

## File Structure

- `internal/client/client.go` *(modify)* — add `respError` helper, `get`/`delete` request helpers, three `List*` and three `Delete*` methods.
- `internal/client/client_test.go` *(append)* — List/Delete tests.
- `internal/apply/apply.go` *(rewrite)* — extend `Cluster`; add `file` to `bundle`; extract `loadBundles`; add `Diff`, the `prune` param on `Apply`, and the `delta`/`desiredKeys`/`liveKeys`/`pruneDrift`/`printPrunePlan`/`printDiff` helpers.
- `internal/apply/apply_test.go` *(rewrite)* — extend the fake `Cluster` (seeded live state + recorded deletes), update existing `Apply` call sites for the new `prune` param, add diff/prune/delta tests.
- `cmd/smithctl/main.go` *(modify)* — add `diff` subcommand, `--prune` flag, and a shared `loadConfig` helper.

**Build-order note:** Task 2 changes the `Apply` signature and its only caller (`cmd/smithctl`) in one commit, so the module always builds.

---

## Task 1: Client List + Delete methods

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Append the failing tests**

Append to `internal/client/client_test.go`:
```go
func TestListWorkloads(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"postgres","image":"postgres:18"},{"id":"deployops","image":"deployops:1"}]`))
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "tok"})
	ws, err := c.ListWorkloads()
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/workloads" {
		t.Errorf("got %s %s, want GET /workloads", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(ws) != 2 || ws[0].ID != "postgres" || ws[1].ID != "deployops" {
		t.Errorf("workloads = %+v", ws)
	}
}

func TestListServicesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	c := New(Config{Server: srv.URL, Token: "t"})
	_, err := c.ListServices()
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want status+body", err)
	}
}

func TestDeleteWorkload(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(Config{Server: srv.URL, Token: "t"})
	if err := c.DeleteWorkload("nginx-test"); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/workloads/nginx-test" {
		t.Errorf("got %s %s, want DELETE /workloads/nginx-test", gotMethod, gotPath)
	}
}

func TestDeleteIngressEscapesAndErrors(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"missing"}`))
	}))
	defer srv.Close()
	c := New(Config{Server: srv.URL, Token: "t"})
	err := c.DeleteIngress("app.kkjorsvik.com")
	if gotPath != "/ingresses/app.kkjorsvik.com" {
		t.Errorf("path = %q, want /ingresses/app.kkjorsvik.com", gotPath)
	}
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want 404 error", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/client/ -run 'TestList|TestDelete' -v`
Expected: compile failure — `c.ListWorkloads undefined` etc.

- [ ] **Step 3: Refactor error mapping and add the methods**

In `internal/client/client.go`, add `"net/url"` to the import block (after `"net/http"`):
```go
	"net/http"
	"net/url"
	"strings"
```

Replace the tail of the `post` method (the success/error block) so it uses a shared helper. Change:
```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(respBody))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (check token): %s", msg)
	}
	return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
}
```
to:
```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return respError(path, resp)
}

// respError reads a non-2xx response body and folds the status into an error.
func respError(path string, resp *http.Response) error {
	respBody, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(respBody))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (check token): %s", msg)
	}
	return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
}

// get issues an authenticated GET and decodes a 2xx JSON body into out.
func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respError(path, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	return nil
}

// del issues an authenticated DELETE; any 2xx is success.
func (c *Client) del(path string) error {
	req, err := http.NewRequest(http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return respError(path, resp)
}

// ListWorkloads returns all workloads via GET /workloads.
func (c *Client) ListWorkloads() ([]types.Workload, error) {
	var out []types.Workload
	if err := c.get("/workloads", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListServices returns all services via GET /services.
func (c *Client) ListServices() ([]types.Service, error) {
	var out []types.Service
	if err := c.get("/services", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListIngresses returns all ingresses via GET /ingresses.
func (c *Client) ListIngresses() ([]types.Ingress, error) {
	var out []types.Ingress
	if err := c.get("/ingresses", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteWorkload removes a workload via DELETE /workloads/{id}.
func (c *Client) DeleteWorkload(id string) error {
	return c.del("/workloads/" + url.PathEscape(id))
}

// DeleteService removes a service via DELETE /services/{name}.
func (c *Client) DeleteService(name string) error {
	return c.del("/services/" + url.PathEscape(name))
}

// DeleteIngress removes an ingress via DELETE /ingresses/{host}.
func (c *Client) DeleteIngress(host string) error {
	return c.del("/ingresses/" + url.PathEscape(host))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/client/ -v`
Expected: PASS (existing post tests + new List/Delete tests).

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): List and Delete methods for diff/prune"
```

---

## Task 2: Diff + prune in apply + smithctl wiring

**Files:**
- Rewrite: `internal/apply/apply.go`
- Rewrite: `internal/apply/apply_test.go`
- Modify: `cmd/smithctl/main.go`

- [ ] **Step 1: Rewrite the test file**

Replace the entire contents of `internal/apply/apply_test.go` with:
```go
package apply

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

// fakeCluster records apply/delete calls in order (deletes prefixed "delete-"),
// captures applied workloads, and serves seeded "live" state for List*.
type fakeCluster struct {
	calls          []string
	workloads      []types.Workload
	failWorkloadID string

	liveWorkloads []types.Workload
	liveServices  []types.Service
	liveIngresses []types.Ingress
}

func (f *fakeCluster) ApplyWorkload(w types.Workload) error {
	f.calls = append(f.calls, "workload:"+w.ID)
	f.workloads = append(f.workloads, w)
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
func (f *fakeCluster) ListWorkloads() ([]types.Workload, error) { return f.liveWorkloads, nil }
func (f *fakeCluster) ListServices() ([]types.Service, error)   { return f.liveServices, nil }
func (f *fakeCluster) ListIngresses() ([]types.Ingress, error)  { return f.liveIngresses, nil }
func (f *fakeCluster) DeleteWorkload(id string) error {
	f.calls = append(f.calls, "delete-workload:"+id)
	return nil
}
func (f *fakeCluster) DeleteService(name string) error {
	f.calls = append(f.calls, "delete-service:"+name)
	return nil
}
func (f *fakeCluster) DeleteIngress(host string) error {
	f.calls = append(f.calls, "delete-ingress:"+host)
	return nil
}

// fakeDecryptor returns a canned env map for any overlay path, or an error.
type fakeDecryptor struct {
	env map[string]string
	err error
}

func (f fakeDecryptor) Decrypt(path string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.env, nil
}

func writeManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const alphaYAML = `name: alpha
workload:
  image: nginx
service:
  port: 80
ingress:
  host: alpha.example.com
`

const betaYAML = `name: beta
workload:
  image: redis
`

const gammaYAML = `name: gamma
workload:
  image: app
  env:
    LOG_LEVEL: info
    DB_PASSWORD: placeholder
`

func TestApplyOrdering(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	writeManifest(t, dir, "beta.yaml", betaYAML)

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, fakeDecryptor{}, false, false, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "workload:alpha,workload:beta,service:alpha,ingress:alpha.example.com"
	if strings.Join(fc.calls, ",") != want {
		t.Errorf("calls = %v, want %v", fc.calls, want)
	}
	if !strings.Contains(out.String(), "applied workload alpha") {
		t.Errorf("output missing applied lines: %q", out.String())
	}
}

func TestApplyValidateAllAborts(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "good.yaml", betaYAML)
	writeManifest(t, dir, "bad.yaml", "name: bad\nworkload: {}\n") // missing image
	fc := &fakeCluster{}
	err := Apply(dir, fc, fakeDecryptor{}, false, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(fc.calls) != 0 {
		t.Errorf("expected zero applies on validation failure, got %v", fc.calls)
	}
}

func TestApplyDryRun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, fakeDecryptor{}, true, false, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("expected zero calls in dry run, got %v", fc.calls)
	}
	if !strings.Contains(out.String(), "plan (dry run") {
		t.Errorf("output missing plan header: %q", out.String())
	}
	for _, want := range []string{"workload alpha", "image=nginx", "service alpha", "port=80", "ingress alpha.example.com -> service alpha"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output missing %q; got:\n%s", want, out.String())
		}
	}
}

func TestApplyFailFast(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	writeManifest(t, dir, "beta.yaml", betaYAML)
	fc := &fakeCluster{failWorkloadID: "beta"}
	err := Apply(dir, fc, fakeDecryptor{}, false, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "service:") || strings.HasPrefix(c, "ingress:") {
			t.Errorf("applied %q after a workload failure", c)
		}
	}
}

func TestApplyNoManifests(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "only.sops.yaml", "env: {}\n")
	err := Apply(dir, &fakeCluster{}, fakeDecryptor{}, false, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no manifests found") {
		t.Fatalf("err = %v, want 'no manifests found'", err)
	}
}

func TestApplyMissingDir(t *testing.T) {
	err := Apply(filepath.Join(t.TempDir(), "nope"), &fakeCluster{}, fakeDecryptor{}, false, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "read dir") {
		t.Fatalf("err = %v, want 'read dir'", err)
	}
}

func TestApplyMergesOverlay(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "gamma.yaml", gammaYAML)
	writeManifest(t, dir, "gamma.sops.yaml", "encrypted-bytes\n")
	dec := fakeDecryptor{env: map[string]string{"DB_PASSWORD": "realsecret", "API_KEY": "k"}}

	fc := &fakeCluster{}
	if err := Apply(dir, fc, dec, false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fc.workloads) != 1 {
		t.Fatalf("got %d workloads, want 1", len(fc.workloads))
	}
	env := fc.workloads[0].Env
	if env["DB_PASSWORD"] != "realsecret" {
		t.Errorf("DB_PASSWORD = %q, want realsecret (overlay should win)", env["DB_PASSWORD"])
	}
	if env["API_KEY"] != "k" {
		t.Errorf("API_KEY = %q, want k (overlay adds new key)", env["API_KEY"])
	}
	if env["LOG_LEVEL"] != "info" {
		t.Errorf("LOG_LEVEL = %q, want info (base env preserved)", env["LOG_LEVEL"])
	}
}

func TestApplyOverlayDecryptErrorAborts(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "gamma.yaml", gammaYAML)
	writeManifest(t, dir, "gamma.sops.yaml", "encrypted-bytes\n")
	dec := fakeDecryptor{err: errors.New("bad key")}

	fc := &fakeCluster{}
	err := Apply(dir, fc, dec, false, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err = %v, want decrypt error", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("expected zero applies on decrypt failure, got %v", fc.calls)
	}
}

func TestApplyDryRunRedactsOverlay(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "gamma.yaml", gammaYAML)
	writeManifest(t, dir, "gamma.sops.yaml", "encrypted-bytes\n")
	dec := fakeDecryptor{env: map[string]string{"DB_PASSWORD": "realsecret"}}

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, dec, true, false, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(out.String(), "realsecret") {
		t.Errorf("dry-run output leaked secret value:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "DB_PASSWORD=(set from overlay)") {
		t.Errorf("dry-run output missing redacted secret key:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "LOG_LEVEL=info") {
		t.Errorf("dry-run output missing base env:\n%s", out.String())
	}
}

func TestApplyOrphanOverlayIgnored(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "beta.yaml", betaYAML)
	writeManifest(t, dir, "orphan.sops.yaml", "env:\n  X: y\n")
	dec := fakeDecryptor{err: errors.New("should not be called")}

	fc := &fakeCluster{}
	if err := Apply(dir, fc, dec, false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(fc.calls, ",") != "workload:beta" {
		t.Errorf("calls = %v, want only workload:beta", fc.calls)
	}
}

func TestDelta(t *testing.T) {
	create, del, inSync := delta([]string{"a", "b", "c"}, []string{"b", "c", "d"})
	if strings.Join(create, ",") != "a" {
		t.Errorf("create = %v, want [a]", create)
	}
	if strings.Join(del, ",") != "d" {
		t.Errorf("del = %v, want [d]", del)
	}
	if strings.Join(inSync, ",") != "b,c" {
		t.Errorf("inSync = %v, want [b c]", inSync)
	}
}

func TestDiff(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	writeManifest(t, dir, "beta.yaml", betaYAML) // git-only -> create
	fc := &fakeCluster{
		liveWorkloads: []types.Workload{{ID: "alpha"}, {ID: "nginx-test"}}, // nginx-test live-only -> delete
		liveServices:  []types.Service{{Name: "alpha"}},
		liveIngresses: []types.Ingress{{Host: "alpha.example.com"}},
	}
	var out bytes.Buffer
	if err := Diff(dir, fc, &out); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	s := out.String()
	for _, want := range []string{"+ create   beta", "- delete   nginx-test", "= in sync  alpha"} {
		if !strings.Contains(s, want) {
			t.Errorf("diff output missing %q; got:\n%s", want, s)
		}
	}
	if len(fc.calls) != 0 {
		t.Errorf("diff must be read-only, recorded %v", fc.calls)
	}
}

func TestApplyPruneDeletesDriftInOrder(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	fc := &fakeCluster{
		liveWorkloads: []types.Workload{{ID: "alpha"}, {ID: "nginx-test"}},
		liveServices:  []types.Service{{Name: "alpha"}, {Name: "nginx-test"}},
		liveIngresses: []types.Ingress{{Host: "alpha.example.com"}, {Host: "old.example.com"}},
	}
	var out bytes.Buffer
	if err := Apply(dir, fc, fakeDecryptor{}, false, true, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "workload:alpha,service:alpha,ingress:alpha.example.com," +
		"delete-ingress:old.example.com,delete-service:nginx-test,delete-workload:nginx-test"
	if strings.Join(fc.calls, ",") != want {
		t.Errorf("calls = %v\nwant %v", fc.calls, want)
	}
}

func TestApplyPruneDryRun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	fc := &fakeCluster{
		liveWorkloads: []types.Workload{{ID: "alpha"}, {ID: "nginx-test"}},
	}
	var out bytes.Buffer
	if err := Apply(dir, fc, fakeDecryptor{}, true, true, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("dry-run prune must not apply or delete, recorded %v", fc.calls)
	}
	if !strings.Contains(out.String(), "delete workload nginx-test") {
		t.Errorf("prune dry-run missing the would-delete line:\n%s", out.String())
	}
}

func TestApplyNoPruneKeepsExtras(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	fc := &fakeCluster{
		liveWorkloads: []types.Workload{{ID: "alpha"}, {ID: "nginx-test"}},
	}
	if err := Apply(dir, fc, fakeDecryptor{}, false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "delete-") {
			t.Errorf("apply without --prune deleted %q", c)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/apply/ 2>&1 | head`
Expected: compile failure — `too many arguments in call to Apply` / `undefined: Diff` / `undefined: delta`.

- [ ] **Step 3: Rewrite `internal/apply/apply.go`**

Replace the entire contents of `internal/apply/apply.go` with:
```go
// Package apply orchestrates GitOps app bundles: it reads a directory of
// manifests, validates every one (merging any sibling <app>.sops.yaml secret
// overlay), and applies the resolved workloads, services, and ingresses through
// a Cluster interface. It can also diff against, and prune drift from, live
// cluster state.
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

// Cluster is the subset of the control-plane API apply needs: apply, list, and
// delete for the three GitOps resource kinds.
type Cluster interface {
	ApplyWorkload(types.Workload) error
	ApplyService(types.Service) error
	ApplyIngress(types.Ingress) error
	ListWorkloads() ([]types.Workload, error)
	ListServices() ([]types.Service, error)
	ListIngresses() ([]types.Ingress, error)
	DeleteWorkload(id string) error
	DeleteService(name string) error
	DeleteIngress(host string) error
}

// Decryptor decrypts a SOPS-encrypted overlay file into its env map. The real
// implementation is internal/secrets.SopsDecryptor; tests use a fake.
type Decryptor interface {
	Decrypt(path string) (map[string]string, error)
}

// bundle pairs a resolved manifest with its source file and the env keys that
// came from its decrypted secret overlay (for dry-run redaction).
type bundle struct {
	file        string
	res         *manifest.Resolved
	overlayKeys map[string]bool
}

// loadBundles reads, parses, and resolves every manifest in dir. It does not
// merge secret overlays (callers that need them do so separately).
func loadBundles(dir string) ([]*bundle, error) {
	files, err := manifestFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no manifests found in %s", dir)
	}
	var bundles []*bundle
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		app, err := manifest.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		res, err := app.Resolve()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		bundles = append(bundles, &bundle{file: file, res: res})
	}
	return bundles, nil
}

// Apply resolves every manifest in dir (merging any sibling secret overlay) and,
// unless dryRun, applies all workloads, then services, then ingresses. When
// prune is set it then deletes live resources not declared in dir, in
// reverse-dependency order. Validation is all-or-nothing: if any manifest is
// invalid or any overlay fails to decrypt, nothing is applied.
func Apply(dir string, cluster Cluster, dec Decryptor, dryRun, prune bool, out io.Writer) error {
	bundles, err := loadBundles(dir)
	if err != nil {
		return err
	}
	for _, b := range bundles {
		keys, err := mergeOverlay(b.file, b.res, dec)
		if err != nil {
			return err
		}
		b.overlayKeys = keys
	}

	if dryRun {
		printPlan(out, bundles)
		if prune {
			return printPrunePlan(cluster, bundles, out)
		}
		return nil
	}

	for _, b := range bundles {
		if err := cluster.ApplyWorkload(b.res.Workload); err != nil {
			return fmt.Errorf("workload %s: %w", b.res.Workload.ID, err)
		}
		fmt.Fprintf(out, "applied workload %s\n", b.res.Workload.ID)
	}
	for _, b := range bundles {
		for _, s := range b.res.Services {
			if err := cluster.ApplyService(s); err != nil {
				return fmt.Errorf("service %s: %w", s.Name, err)
			}
			fmt.Fprintf(out, "applied service %s\n", s.Name)
		}
	}
	for _, b := range bundles {
		for _, in := range b.res.Ingresses {
			if err := cluster.ApplyIngress(in); err != nil {
				return fmt.Errorf("ingress %s: %w", in.Host, err)
			}
			fmt.Fprintf(out, "applied ingress %s\n", in.Host)
		}
	}

	if prune {
		return pruneDrift(cluster, bundles, out)
	}
	return nil
}

// Diff resolves the manifests in dir and prints the create/delete/in-sync delta
// against live cluster state, by resource identity. It is read-only and does not
// decrypt secret overlays (it compares identities, not env contents).
func Diff(dir string, cluster Cluster, out io.Writer) error {
	bundles, err := loadBundles(dir)
	if err != nil {
		return err
	}
	dw, ds, di := desiredKeys(bundles)
	lw, ls, li, err := liveKeys(cluster)
	if err != nil {
		return err
	}
	printDiff(out, "workloads", dw, lw)
	printDiff(out, "services", ds, ls)
	printDiff(out, "ingresses", di, li)
	return nil
}

// desiredKeys returns the workload IDs, service names, and ingress hosts the
// bundles declare.
func desiredKeys(bundles []*bundle) (workloads, services, ingresses []string) {
	for _, b := range bundles {
		workloads = append(workloads, b.res.Workload.ID)
		for _, s := range b.res.Services {
			services = append(services, s.Name)
		}
		for _, in := range b.res.Ingresses {
			ingresses = append(ingresses, in.Host)
		}
	}
	return workloads, services, ingresses
}

// liveKeys lists the live resource keys from the cluster.
func liveKeys(cluster Cluster) (workloads, services, ingresses []string, err error) {
	ws, err := cluster.ListWorkloads()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list workloads: %w", err)
	}
	ss, err := cluster.ListServices()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list services: %w", err)
	}
	is, err := cluster.ListIngresses()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list ingresses: %w", err)
	}
	for _, w := range ws {
		workloads = append(workloads, w.ID)
	}
	for _, s := range ss {
		services = append(services, s.Name)
	}
	for _, in := range is {
		ingresses = append(ingresses, in.Host)
	}
	return workloads, services, ingresses, nil
}

// delta partitions desired and live keys into create (desired-only), del
// (live-only), and inSync (in both). All results are sorted.
func delta(desired, live []string) (create, del, inSync []string) {
	desiredSet := make(map[string]bool, len(desired))
	for _, k := range desired {
		desiredSet[k] = true
	}
	liveSet := make(map[string]bool, len(live))
	for _, k := range live {
		liveSet[k] = true
	}
	for _, k := range desired {
		if liveSet[k] {
			inSync = append(inSync, k)
		} else {
			create = append(create, k)
		}
	}
	for _, k := range live {
		if !desiredSet[k] {
			del = append(del, k)
		}
	}
	sort.Strings(create)
	sort.Strings(del)
	sort.Strings(inSync)
	return create, del, inSync
}

// pruneDrift deletes live resources not declared by the bundles, in
// reverse-dependency order: ingresses, then services, then workloads.
func pruneDrift(cluster Cluster, bundles []*bundle, out io.Writer) error {
	dw, ds, di := desiredKeys(bundles)
	lw, ls, li, err := liveKeys(cluster)
	if err != nil {
		return err
	}
	_, delIng, _ := delta(di, li)
	_, delSvc, _ := delta(ds, ls)
	_, delWl, _ := delta(dw, lw)

	for _, h := range delIng {
		if err := cluster.DeleteIngress(h); err != nil {
			return fmt.Errorf("prune ingress %s: %w", h, err)
		}
		fmt.Fprintf(out, "pruned ingress %s\n", h)
	}
	for _, n := range delSvc {
		if err := cluster.DeleteService(n); err != nil {
			return fmt.Errorf("prune service %s: %w", n, err)
		}
		fmt.Fprintf(out, "pruned service %s\n", n)
	}
	for _, id := range delWl {
		if err := cluster.DeleteWorkload(id); err != nil {
			return fmt.Errorf("prune workload %s: %w", id, err)
		}
		fmt.Fprintf(out, "pruned workload %s\n", id)
	}
	return nil
}

// printPrunePlan prints the resources prune would delete, without deleting them.
func printPrunePlan(cluster Cluster, bundles []*bundle, out io.Writer) error {
	dw, ds, di := desiredKeys(bundles)
	lw, ls, li, err := liveKeys(cluster)
	if err != nil {
		return err
	}
	_, delIng, _ := delta(di, li)
	_, delSvc, _ := delta(ds, ls)
	_, delWl, _ := delta(dw, lw)

	fmt.Fprintln(out, "prune (dry run, nothing deleted):")
	for _, h := range delIng {
		fmt.Fprintf(out, "  - delete ingress %s\n", h)
	}
	for _, n := range delSvc {
		fmt.Fprintf(out, "  - delete service %s\n", n)
	}
	for _, id := range delWl {
		fmt.Fprintf(out, "  - delete workload %s\n", id)
	}
	return nil
}

// printDiff prints one kind's create/delete/in-sync delta.
func printDiff(out io.Writer, kind string, desired, live []string) {
	create, del, inSync := delta(desired, live)
	fmt.Fprintf(out, "%s:\n", kind)
	for _, k := range create {
		fmt.Fprintf(out, "  + create   %s\n", k)
	}
	for _, k := range del {
		fmt.Fprintf(out, "  - delete   %s\n", k)
	}
	if len(inSync) > 0 {
		fmt.Fprintf(out, "  = in sync  %s\n", strings.Join(inSync, ", "))
	}
}

// mergeOverlay looks for a sibling <base>.sops.yaml next to the bundle file; if
// present it decrypts it and merges its env over the workload env (overlay
// wins). It returns the set of keys that came from the overlay (for dry-run
// redaction), or nil if there is no overlay.
func mergeOverlay(bundleFile string, res *manifest.Resolved, dec Decryptor) (map[string]bool, error) {
	overlayPath := strings.TrimSuffix(bundleFile, ".yaml") + ".sops.yaml"
	if _, err := os.Stat(overlayPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: stat overlay: %w", res.Workload.ID, err)
	}

	env, err := dec.Decrypt(overlayPath)
	if err != nil {
		return nil, fmt.Errorf("%s: decrypt %s: %w", res.Workload.ID, overlayPath, err)
	}

	if res.Workload.Env == nil {
		res.Workload.Env = make(map[string]string, len(env))
	}
	keys := make(map[string]bool, len(env))
	for k, v := range env {
		res.Workload.Env[k] = v // overlay wins
		keys[k] = true
	}
	return keys, nil
}

// manifestFiles returns the sorted list of *.yaml files in dir, excluding
// *.sops.yaml encrypted secrets and any subdirectories.
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

// printPlan writes a human-readable dry-run summary of what would be applied.
// Workload env is shown with overlay-sourced keys redacted so secret values are
// never printed.
func printPlan(out io.Writer, bundles []*bundle) {
	fmt.Fprintln(out, "plan (dry run, nothing applied):")
	for _, b := range bundles {
		w := b.res.Workload
		fmt.Fprintf(out, "  workload %s  image=%s replicas=%d\n", w.ID, w.Image, w.Replicas)
		for _, k := range sortedKeys(w.Env) {
			if b.overlayKeys[k] {
				fmt.Fprintf(out, "    env %s=(set from overlay)\n", k)
			} else {
				fmt.Fprintf(out, "    env %s=%s\n", k, w.Env[k])
			}
		}
	}
	for _, b := range bundles {
		for _, s := range b.res.Services {
			fmt.Fprintf(out, "  service %s  port=%d nodePort=%d\n", s.Name, s.Port, s.NodePort)
		}
	}
	for _, b := range bundles {
		for _, in := range b.res.Ingresses {
			fmt.Fprintf(out, "  ingress %s -> service %s\n", in.Host, in.Service)
		}
	}
}

// sortedKeys returns the keys of m in sorted order for deterministic output.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

- [ ] **Step 4: Wire the smithctl CLI**

Replace the entire contents of `cmd/smithctl/main.go` with:
```go
// Command smithctl is the smith operator CLI. It resolves a directory of GitOps
// app bundles and applies (or diffs/prunes) them against the control-plane API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kkjorsvik/smith/internal/apply"
	"github.com/kkjorsvik/smith/internal/client"
	"github.com/kkjorsvik/smith/internal/secrets"
)

const usage = "usage: smithctl [--config PATH] <apply|diff> [flags] <dir>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "smithctl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("smithctl", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the smithctl config file")
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
	case "diff":
		return runDiff(*configPath, rest[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", rest[0], usage)
	}
}

func runApply(configPath string, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "validate and print a plan without applying")
	prune := fs.Bool("prune", false, "delete live resources not declared in <dir>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: smithctl apply [--dry-run] [--prune] <dir>")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	return apply.Apply(fs.Arg(0), client.New(cfg), secrets.SopsDecryptor{}, *dryRun, *prune, os.Stdout)
}

func runDiff(configPath string, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: smithctl diff <dir>")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	return apply.Diff(fs.Arg(0), client.New(cfg), os.Stdout)
}

// loadConfig resolves the config path (defaulting when empty) and loads it.
func loadConfig(configPath string) (client.Config, error) {
	if configPath == "" {
		p, err := client.DefaultConfigPath()
		if err != nil {
			return client.Config{}, err
		}
		configPath = p
	}
	return client.LoadConfig(configPath)
}
```

- [ ] **Step 5: Run tests and build to verify they pass**

Run: `go test ./internal/apply/ -v && go build ./...`
Expected: all apply tests PASS (including the new diff/prune/delta tests); `go build ./...` succeeds.

- [ ] **Step 6: Smoke-test the new CLI surface (no cluster needed)**

Run:
```bash
go build -o bin/smithctl ./cmd/smithctl
./bin/smithctl 2>&1; echo "---"; ./bin/smithctl --config /nonexistent diff ./x 2>&1
```
Expected: first prints the usage line (mentioning `apply|diff`) and exits non-zero; second prints `smithctl: no config at /nonexistent; set server and token`.

- [ ] **Step 7: Commit**

```bash
git add internal/apply/apply.go internal/apply/apply_test.go cmd/smithctl/main.go
git commit -m "feat(apply): smithctl diff and apply --prune"
```

---

## Task 3: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the touched packages' tests**

Run: `go test ./internal/client/ ./internal/apply/ -v`
Expected: PASS for all.

- [ ] **Step 2: Vet and full build**

Run: `go vet ./internal/client/ ./internal/apply/ ./cmd/smithctl/ && go build ./...`
Expected: no vet output; build succeeds.

- [ ] **Step 3: Full test suite**

Run: `go test ./...`
Expected: every package passes (or `[no test files]`); nothing fails.

- [ ] **Step 4: Commit (only if vet/build required a fix; otherwise skip)**

```bash
git commit -am "chore(smithctl): vet/build clean"
```

---

## Self-Review Notes (author)

Spec coverage check against `2026-06-25-gitops-diff-prune-design.md`:

- `ListWorkloads/Services/Ingresses` + `DeleteWorkload/Service/Ingress` on the client, reusing error handling → Task 1.
- `Cluster` grows to nine methods → Task 2 (`apply.go`).
- `Diff` read-only, identity-only, no decryption → Task 2 `Diff` + `TestDiff`.
- `Apply` gains `prune`; applies desired first, then deletes `live − desired` in reverse-dependency order → Task 2 `pruneDrift` + `TestApplyPruneDeletesDriftInOrder`.
- `apply --prune --dry-run` prints deletes, deletes nothing → `printPrunePlan` + `TestApplyPruneDryRun`.
- `apply` without prune never deletes → `TestApplyNoPruneKeepsExtras`.
- Shared `delta` helper, sorted output → `delta` + `TestDelta`; diff output format → `printDiff` + `TestDiff`.
- `smithctl diff` subcommand + `--prune` flag → Task 2 Step 4; smoke-tested in Step 6.

Out of scope by design (later/never): deep field-level update diff, label-based ownership, confirmation prompts, the in-cluster pull-loop.

**Manual follow-up (not a code task):** after this lands, `smithctl diff ../smith-cluster/apps` should show everything `= in sync` (the live cluster currently matches the two manifests), confirming a real prune would be a no-op.
