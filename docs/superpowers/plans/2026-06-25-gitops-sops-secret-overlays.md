# GitOps SOPS Secret Overlays Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decrypt a sibling `<app>.sops.yaml` overlay with the `sops` binary and merge its env over the workload at apply time (overlay wins), so a real `smithctl apply` is safe for secret-bearing apps; `--dry-run` decrypts and redacts as a rehearsal.

**Architecture:** A new `internal/secrets` package shells out to `sops --decrypt` and returns an env map. `internal/apply` gains a `Decryptor` interface (consumer-side, like `Cluster`); during its existing validate-all pre-flight it looks for the sibling overlay, decrypts, merges over `workload.Env`, and records which keys came from the overlay so the dry-run plan can redact them. `cmd/smithctl` wires the real `secrets.SopsDecryptor`.

**Tech Stack:** Go 1.26, `os/exec` (shell out to `sops`), `sigs.k8s.io/yaml` (already a dependency), the existing `internal/apply`/`internal/client`/`internal/manifest`/`internal/types` packages.

**Spec:** `docs/superpowers/specs/2026-06-25-gitops-sops-secret-overlays-design.md`

---

## File Structure

- `internal/secrets/sops.go` *(new)* — `SopsDecryptor`, `Decrypt(path)` (exec sops + parse), `parseOverlay` helper. One job: encrypted file → env map.
- `internal/secrets/sops_test.go` *(new)* — `parseOverlay` unit tests + a sops-not-installed test (no real sops needed).
- `internal/apply/apply.go` *(modify)* — add `Decryptor` interface, `Decryptor` param on `Apply`, `mergeOverlay`, per-workload overlay-key tracking, redacted env in `printPlan`.
- `internal/apply/apply_test.go` *(rewrite)* — add fake `Decryptor`, capture applied workloads, update existing call sites to the new signature, add overlay tests.
- `cmd/smithctl/main.go` *(modify)* — pass `secrets.SopsDecryptor{}` to `apply.Apply`.

**Build-order note:** Task 2 changes the `Apply` signature and its only caller (`cmd/smithctl`) together in one commit, so the module always builds. `internal/secrets` (Task 1) lands first because `main.go` imports it.

---

## Task 1: `internal/secrets` — sops decryptor

**Files:**
- Create: `internal/secrets/sops.go`
- Test: `internal/secrets/sops_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/secrets/sops_test.go`:
```go
package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOverlayValid(t *testing.T) {
	env, err := parseOverlay([]byte("env:\n  POSTGRES_PASSWORD: secret\n  API_KEY: k\n"))
	if err != nil {
		t.Fatalf("parseOverlay: %v", err)
	}
	if env["POSTGRES_PASSWORD"] != "secret" || env["API_KEY"] != "k" {
		t.Errorf("env = %v", env)
	}
}

func TestParseOverlayRejectsUnknownKey(t *testing.T) {
	// The decrypted overlay must be an {env: map} document; anything else is a
	// mistake we want surfaced.
	_, err := parseOverlay([]byte("secrets:\n  X: y\n"))
	if err == nil {
		t.Fatal("expected error for non-env document, got nil")
	}
}

func TestDecryptSopsNotInstalled(t *testing.T) {
	// Empty PATH so the sops binary cannot be found; Decrypt must say so clearly.
	t.Setenv("PATH", "")
	f := filepath.Join(t.TempDir(), "x.sops.yaml")
	if err := os.WriteFile(f, []byte("env: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := SopsDecryptor{}.Decrypt(f)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want 'not installed'", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/secrets/ -v`
Expected: compile failure — `undefined: parseOverlay` / `undefined: SopsDecryptor`.

- [ ] **Step 3: Write the implementation**

Create `internal/secrets/sops.go`:
```go
// Package secrets decrypts SOPS-encrypted env overlays by shelling out to the
// sops binary. The apply engine uses it to merge secret env into a workload at
// apply time. sops handles age-key discovery via its own conventions
// (SOPS_AGE_KEY_FILE or ~/.config/sops/age/keys.txt).
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"sigs.k8s.io/yaml"
)

// SopsDecryptor decrypts overlay files using the `sops` binary on PATH.
type SopsDecryptor struct{}

// overlay is the decrypted shape of a *.sops.yaml file: just an env map.
type overlay struct {
	Env map[string]string `json:"env"`
}

// Decrypt runs `sops --decrypt` on path and returns its env map. It returns a
// clear error if sops is not installed, if decryption fails, or if the
// decrypted content is not an {env: map} document.
func (SopsDecryptor) Decrypt(path string) (map[string]string, error) {
	cmd := exec.Command("sops", "--decrypt", "--output-type", "yaml", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("'sops' is not installed (PATH)")
		}
		return nil, fmt.Errorf("sops decrypt failed: %s", strings.TrimSpace(stderr.String()))
	}
	return parseOverlay(stdout.Bytes())
}

// parseOverlay decodes decrypted sops YAML (an {env: map} document) into a flat
// env map, rejecting any other shape.
func parseOverlay(data []byte) (map[string]string, error) {
	var o overlay
	if err := yaml.UnmarshalStrict(data, &o); err != nil {
		return nil, fmt.Errorf("parse overlay: %w", err)
	}
	return o.Env, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/secrets/ -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/sops.go internal/secrets/sops_test.go
git commit -m "feat(secrets): sops decryptor for encrypted env overlays"
```

---

## Task 2: Overlay decrypt/merge in apply + wire into smithctl

**Files:**
- Modify: `internal/apply/apply.go`
- Rewrite: `internal/apply/apply_test.go`
- Modify: `cmd/smithctl/main.go`

- [ ] **Step 1: Rewrite the test file with new + updated tests**

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

// fakeCluster records the ordered apply calls and captures applied workloads so
// tests can inspect merged env. It can be told to fail on a specific workload.
type fakeCluster struct {
	calls          []string
	workloads      []types.Workload
	failWorkloadID string
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
	if err := Apply(dir, fc, fakeDecryptor{}, false, &out); err != nil {
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
	err := Apply(dir, fc, fakeDecryptor{}, false, &bytes.Buffer{})
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
	if err := Apply(dir, fc, fakeDecryptor{}, true, &out); err != nil {
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
	err := Apply(dir, fc, fakeDecryptor{}, false, &bytes.Buffer{})
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
	err := Apply(dir, &fakeCluster{}, fakeDecryptor{}, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no manifests found") {
		t.Fatalf("err = %v, want 'no manifests found'", err)
	}
}

func TestApplyMissingDir(t *testing.T) {
	err := Apply(filepath.Join(t.TempDir(), "nope"), &fakeCluster{}, fakeDecryptor{}, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "read dir") {
		t.Fatalf("err = %v, want 'read dir'", err)
	}
}

func TestApplyMergesOverlay(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "gamma.yaml", gammaYAML)
	writeManifest(t, dir, "gamma.sops.yaml", "encrypted-bytes\n") // content ignored by fake
	dec := fakeDecryptor{env: map[string]string{"DB_PASSWORD": "realsecret", "API_KEY": "k"}}

	fc := &fakeCluster{}
	if err := Apply(dir, fc, dec, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fc.workloads) != 1 {
		t.Fatalf("got %d workloads, want 1", len(fc.workloads))
	}
	env := fc.workloads[0].Env
	if env["DB_PASSWORD"] != "realsecret" { // overlay wins over the placeholder
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
	err := Apply(dir, fc, dec, false, &bytes.Buffer{})
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
	if err := Apply(dir, fc, dec, true, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("dry run applied resources: %v", fc.calls)
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
	writeManifest(t, dir, "orphan.sops.yaml", "env:\n  X: y\n") // no orphan.yaml bundle
	// Decryptor would error if called, proving the orphan is never decrypted.
	dec := fakeDecryptor{err: errors.New("should not be called")}

	fc := &fakeCluster{}
	if err := Apply(dir, fc, dec, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(fc.calls, ",") != "workload:beta" {
		t.Errorf("calls = %v, want only workload:beta", fc.calls)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/apply/ 2>&1 | head`
Expected: compile failure — `too many arguments in call to Apply` / `undefined` (the new signature and helpers don't exist yet).

- [ ] **Step 3: Rewrite `internal/apply/apply.go`**

Replace the entire contents of `internal/apply/apply.go` with:
```go
// Package apply orchestrates GitOps app bundles: it reads a directory of
// manifests, validates every one (merging any sibling <app>.sops.yaml secret
// overlay) before touching the cluster, then applies the resolved workloads,
// services, and ingresses through a Cluster interface.
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

// Cluster is the subset of the control-plane API apply needs: it creates or
// updates the three GitOps resource kinds.
type Cluster interface {
	ApplyWorkload(types.Workload) error
	ApplyService(types.Service) error
	ApplyIngress(types.Ingress) error
}

// Decryptor decrypts a SOPS-encrypted overlay file into its env map. The real
// implementation is internal/secrets.SopsDecryptor; tests use a fake.
type Decryptor interface {
	Decrypt(path string) (map[string]string, error)
}

// bundle pairs a resolved manifest with the env keys that came from its
// decrypted secret overlay, so a dry run can redact those values.
type bundle struct {
	res         *manifest.Resolved
	overlayKeys map[string]bool
}

// Apply reads every manifest in dir, resolves and validates them all (merging
// any sibling <app>.sops.yaml secret overlay into the workload env), and then
// (unless dryRun) applies all workloads, then all services, then all ingresses.
// Validation is all-or-nothing: if any manifest is invalid or any overlay fails
// to decrypt, nothing is applied.
func Apply(dir string, cluster Cluster, dec Decryptor, dryRun bool, out io.Writer) error {
	files, err := manifestFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no manifests found in %s", dir)
	}

	var bundles []*bundle
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		app, err := manifest.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		res, err := app.Resolve()
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		overlayKeys, err := mergeOverlay(file, res, dec)
		if err != nil {
			return err
		}
		bundles = append(bundles, &bundle{res: res, overlayKeys: overlayKeys})
	}

	if dryRun {
		printPlan(out, bundles)
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
	return nil
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

- [ ] **Step 4: Update the smithctl call site**

In `cmd/smithctl/main.go`, add the import and pass the decryptor. Change the import block to add `secrets`:
```go
	"github.com/kkjorsvik/smith/internal/apply"
	"github.com/kkjorsvik/smith/internal/client"
	"github.com/kkjorsvik/smith/internal/secrets"
```
And change the final line of `runApply` from:
```go
	return apply.Apply(dir, client.New(cfg), *dryRun, os.Stdout)
```
to:
```go
	return apply.Apply(dir, client.New(cfg), secrets.SopsDecryptor{}, *dryRun, os.Stdout)
```

- [ ] **Step 5: Run tests and build to verify they pass**

Run: `go test ./internal/apply/ -v && go build ./...`
Expected: all apply tests PASS (including the new overlay tests); `go build ./...` succeeds (main.go now matches the new signature).

- [ ] **Step 6: Commit**

```bash
git add internal/apply/apply.go internal/apply/apply_test.go cmd/smithctl/main.go
git commit -m "feat(apply): decrypt and merge sops secret overlays at apply time"
```

---

## Task 3: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the new and touched packages' tests**

Run: `go test ./internal/secrets/ ./internal/apply/ -v`
Expected: PASS for all.

- [ ] **Step 2: Vet and full build**

Run: `go vet ./internal/secrets/ ./internal/apply/ ./cmd/smithctl/ && go build ./...`
Expected: no vet output; build succeeds.

- [ ] **Step 3: Full test suite**

Run: `go test ./...`
Expected: every package passes (or `[no test files]`); nothing fails.

- [ ] **Step 4: Commit (only if vet/build required a fix; otherwise skip)**

```bash
git commit -am "chore(secrets): vet/build clean"
```

---

## Self-Review Notes (author)

Spec coverage check against `2026-06-25-gitops-sops-secret-overlays-design.md`:

- New `internal/secrets` shelling out to `sops --decrypt --output-type yaml`, parsing the env map → Task 1.
- `Decryptor` interface in `internal/apply` (consumer side), `Apply` gains the param → Task 2.
- Pre-flight discovery of sibling `<base>.sops.yaml`, decrypt, merge-overlay-wins, record overlay keys → Task 2 `mergeOverlay` + `TestApplyMergesOverlay`.
- Decrypt failure aborts with zero applies (validate-all guarantee) → Task 2 `TestApplyOverlayDecryptErrorAborts`.
- `--dry-run` decrypt + redact, value never printed → Task 2 `printPlan` + `TestApplyDryRunRedactsOverlay`.
- Orphan overlay ignored → Task 2 `TestApplyOrphanOverlayIgnored`.
- sops-not-installed clear error → Task 1 `TestDecryptSopsNotInstalled`.
- Deterministic dry-run output (sorted env keys) → Task 2 `sortedKeys`.
- smithctl wires the real decryptor → Task 2 Step 4.

Deliberate, minor deviations from the spec's exact error wording: the spec table phrased three distinct messages ("found <file> but 'sops' is not installed", etc.). The implementation surfaces the same information composed from two layers — `internal/secrets` returns the root cause (`'sops' is not installed (PATH)`, `sops decrypt failed: <stderr>`, `parse overlay: ...`) and `internal/apply` wraps with `<app>: decrypt <overlay>: %w`. The combined messages are actionable and name the app + overlay; exact phrasing differs. (Not a gap — noted so a reviewer isn't surprised.)

Out of scope by design (later specs/manual): pruning, diff-vs-live, the pull-loop, and `smithctl` encrypting/editing secrets. Producing the actual encrypted `apps/postgres.sops.yaml` / `apps/deployops.sops.yaml` is an operator step (age key + `sops`), per the spec's operator-workflow section — not part of this code plan.

**Manual follow-up after this lands (not a code task):** generate an age key, add `.sops.yaml` creation rules to the `smith-cluster` repo, `sops` the two overlay files, then `smithctl apply --dry-run` to verify the full decrypt+redact rehearsal end to end before a real apply.
