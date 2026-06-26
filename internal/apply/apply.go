// Package apply orchestrates GitOps app bundles: it reads a directory of
// manifests, validates every one before touching the cluster, then applies the
// resolved workloads, services, and ingresses through a Cluster interface.
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

// Apply reads every manifest in dir, resolves and validates them all, and then
// (unless dryRun) applies all workloads, then all services, then all ingresses.
// Validation is all-or-nothing: if any manifest is invalid, nothing is applied.
func Apply(dir string, cluster Cluster, dryRun bool, out io.Writer) error {
	files, err := manifestFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no manifests found in %s", dir)
	}

	var resolved []*manifest.Resolved
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
		resolved = append(resolved, res)
	}

	if dryRun {
		printPlan(out, resolved)
		return nil
	}

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
func printPlan(out io.Writer, resolved []*manifest.Resolved) {
	fmt.Fprintln(out, "plan (dry run, nothing applied):")
	for _, r := range resolved {
		w := r.Workload
		fmt.Fprintf(out, "  workload %s  image=%s replicas=%d\n", w.ID, w.Image, w.Replicas)
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
