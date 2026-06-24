package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// NFSMountRoot is where the agent mounts the cluster NFS share. Per-workload
// volume directories live beneath it as <workloadID>/<volumeName>. It is a var
// (not a const) so tests can redirect it to a temp dir.
var NFSMountRoot = "/var/lib/smith/nfs"

// MountNFS mounts source (e.g. "unraid.kkjorsvik.com:/mnt/user/smith") at
// NFSMountRoot. Idempotent: a no-op if NFSMountRoot is already a mount point.
// Requires the nfs-common mount helper (mount.nfs) on the host.
func MountNFS(source string) error {
	if err := os.MkdirAll(NFSMountRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", NFSMountRoot, err)
	}

	mounted, err := isMountpoint(NFSMountRoot)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	out, err := exec.Command("mount", "-t", "nfs", source, NFSMountRoot).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount nfs %s at %s: %w (%s)", source, NFSMountRoot, err, out)
	}
	return nil
}

// EnsureVolumeDir creates and returns the host path backing a workload volume:
// <NFSMountRoot>/<baseID>/<name>. The directory is created on the NFS share if
// absent and reused (data-preserving) if present.
//
// The mount dir is made world-writable (0777). Containers commonly run as a
// non-root uid (postgres=999, etc.) — often re-exec'ing from root to that uid
// mid-entrypoint — and must be able to write into their volume; the orchestrator
// doesn't know that uid, so the dir is opened up and the app manages the perms
// of its own data dir inside (Postgres chmods PGDATA to 0700 itself). The chmod
// is explicit because MkdirAll is subject to umask, and it also corrects dirs
// created 0700 by an older agent.
func EnsureVolumeDir(baseID, name string) (string, error) {
	dir := filepath.Join(NFSMountRoot, baseID, name)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", fmt.Errorf("ensure volume dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return "", fmt.Errorf("chmod volume dir %s: %w", dir, err)
	}
	return dir, nil
}

// isMountpoint reports whether path is a mount point, by comparing its device
// number with its parent's (they differ across a mount boundary).
func isMountpoint(path string) (bool, error) {
	out, err := exec.Command("mountpoint", "-q", path).CombinedOutput()
	if err == nil {
		return true, nil
	}
	// mountpoint exits non-zero when path is not a mount point; that's the
	// common, expected case, not an error.
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil
	}
	return false, fmt.Errorf("mountpoint %s: %w (%s)", path, err, out)
}
