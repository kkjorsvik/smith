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

// prunePlan computes the live resource keys not declared by the bundles, per
// kind. Both pruneDrift and printPrunePlan use it so the dry-run preview can
// never diverge from what a real prune deletes.
func prunePlan(cluster Cluster, bundles []*bundle) (delIng, delSvc, delWl []string, err error) {
	dw, ds, di := desiredKeys(bundles)
	lw, ls, li, err := liveKeys(cluster)
	if err != nil {
		return nil, nil, nil, err
	}
	_, delIng, _ = delta(di, li)
	_, delSvc, _ = delta(ds, ls)
	_, delWl, _ = delta(dw, lw)
	return delIng, delSvc, delWl, nil
}

// pruneDrift deletes live resources not declared by the bundles, in
// reverse-dependency order: ingresses, then services, then workloads.
func pruneDrift(cluster Cluster, bundles []*bundle, out io.Writer) error {
	delIng, delSvc, delWl, err := prunePlan(cluster, bundles)
	if err != nil {
		return err
	}

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
	delIng, delSvc, delWl, err := prunePlan(cluster, bundles)
	if err != nil {
		return err
	}

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
