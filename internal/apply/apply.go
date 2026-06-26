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
