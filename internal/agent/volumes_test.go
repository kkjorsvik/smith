package agent

import (
	"os"
	"path/filepath"
	"testing"

	smithruntime "github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/types"
)

func TestResolveVolumes(t *testing.T) {
	// Redirect the NFS root to a writable temp dir for the happy path.
	tmp := t.TempDir()
	orig := smithruntime.NFSMountRoot
	smithruntime.NFSMountRoot = tmp
	defer func() { smithruntime.NFSMountRoot = orig }()

	// No volumes -> no mounts, no error, regardless of NFS config.
	if m, err := (&Agent{}).resolveVolumes(types.Workload{ID: "web"}); err != nil || m != nil {
		t.Fatalf("stateless: got (%v, %v), want (nil, nil)", m, err)
	}

	// Volumes but no NFS share -> error (assign fails).
	stateful := types.Workload{ID: "pg-0", Volumes: []types.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}}
	if _, err := (&Agent{}).resolveVolumes(stateful); err == nil {
		t.Fatal("volumes without NFS share should error")
	}

	// Volumes with NFS share -> bind mount under <root>/<parentID>/<name>.
	// The replica id "pg-0" must resolve to the parent dir "pg" so data is
	// stable across recreation/failover.
	a := &Agent{nfsSource: "unraid:/mnt/user/smith"}
	mounts, err := a.resolveVolumes(stateful)
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	wantSrc := filepath.Join(tmp, "pg", "data")
	if mounts[0].Source != wantSrc {
		t.Errorf("source = %q, want %q", mounts[0].Source, wantSrc)
	}
	if mounts[0].Dest != "/var/lib/postgresql/data" {
		t.Errorf("dest = %q, want /var/lib/postgresql/data", mounts[0].Dest)
	}

	// The mount dir must be world-writable so a container running as a non-root
	// uid (e.g. postgres=999) can write into its volume.
	info, err := os.Stat(mounts[0].Source)
	if err != nil {
		t.Fatalf("stat volume dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o777 {
		t.Errorf("volume dir perms = %o, want 777", perm)
	}
}

func TestReplicaSuffixStripping(t *testing.T) {
	cases := map[string]string{
		"pg-0":          "pg",
		"smith-nginx-0": "smith-nginx",
		"db-12":         "db",
	}
	for in, want := range cases {
		if got := replicaSuffixRe.ReplaceAllString(in, ""); got != want {
			t.Errorf("strip %q = %q, want %q", in, got, want)
		}
	}
}
