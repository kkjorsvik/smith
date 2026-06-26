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

// fakeCluster records the Apply calls it receives, in order, and can be told to
// fail a specific workload to exercise fail-fast behavior.
type fakeCluster struct {
	calls          []string
	failWorkloadID string
}

func (f *fakeCluster) ApplyWorkload(w types.Workload) error {
	if w.ID == f.failWorkloadID {
		return fmt.Errorf("boom")
	}
	f.calls = append(f.calls, "workload:"+w.ID)
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
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest %s: %v", name, err)
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

func TestApplyOrdering(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)
	writeManifest(t, dir, "beta.yaml", betaYAML)

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, false, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{"workload:alpha", "workload:beta", "service:alpha", "ingress:alpha.example.com"}
	if strings.Join(fc.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", fc.calls, want)
	}
	if !strings.Contains(out.String(), "applied workload alpha") {
		t.Errorf("output missing applied workload alpha: %q", out.String())
	}
}

func TestApplyValidateAllAborts(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "good.yaml", betaYAML)
	writeManifest(t, dir, "bad.yaml", "name: bad\nworkload: {}\n")

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, false, &out); err == nil {
		t.Fatal("expected error from invalid manifest")
	}
	if len(fc.calls) != 0 {
		t.Errorf("expected zero calls, got %v", fc.calls)
	}
}

func TestApplyDryRun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha.yaml", alphaYAML)

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, true, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("expected zero calls in dry run, got %v", fc.calls)
	}
	if !strings.Contains(out.String(), "plan (dry run") {
		t.Errorf("output missing plan header: %q", out.String())
	}
	// The plan must show the resolved resources, not just the header.
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
	var out bytes.Buffer
	if err := Apply(dir, fc, false, &out); err == nil {
		t.Fatal("expected error from failed workload")
	}
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "service:") || strings.HasPrefix(c, "ingress:") {
			t.Errorf("expected no service/ingress calls after workload failure, got %v", fc.calls)
		}
	}
}

func TestApplySkipsSopsFiles(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "beta.yaml", betaYAML)
	writeManifest(t, dir, "beta.sops.yaml", "garbage that should never parse")

	fc := &fakeCluster{}
	var out bytes.Buffer
	if err := Apply(dir, fc, false, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(fc.calls, ",") != "workload:beta" {
		t.Errorf("calls = %v, want [workload:beta]", fc.calls)
	}
}

func TestApplyNoManifests(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "secret.sops.yaml", "x: y")

	fc := &fakeCluster{}
	var out bytes.Buffer
	err := Apply(dir, fc, false, &out)
	if err == nil {
		t.Fatal("expected error when no manifests found")
	}
	if !strings.Contains(err.Error(), "no manifests found") {
		t.Errorf("error %q does not mention no manifests found", err)
	}
}

func TestApplyMissingDir(t *testing.T) {
	fc := &fakeCluster{}
	var out bytes.Buffer
	err := Apply(filepath.Join(t.TempDir(), "nope"), fc, false, &out)
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
	if !strings.Contains(err.Error(), "read dir") {
		t.Errorf("error %q does not mention read dir", err)
	}
}
