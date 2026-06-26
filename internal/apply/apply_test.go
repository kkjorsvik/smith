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
