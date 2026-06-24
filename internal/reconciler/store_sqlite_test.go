package reconciler

import (
	"path/filepath"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestWorkloadVolumesRoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	want := types.Workload{
		ID:       "pg",
		Image:    "docker.io/library/postgres:16",
		Args:     []string{},
		Replicas: 1,
		Volumes: []types.Volume{
			{Name: "data", Path: "/var/lib/postgresql/data"},
		},
	}
	if err := store.Add(want); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wl, ok := got["pg"]
	if !ok {
		t.Fatal("workload pg not found after Add")
	}
	if len(wl.Volumes) != 1 || wl.Volumes[0] != want.Volumes[0] {
		t.Fatalf("volumes round-trip mismatch: got %+v, want %+v", wl.Volumes, want.Volumes)
	}
}

func TestSpecHashIncludesVolumes(t *testing.T) {
	base := types.Workload{ID: "pg", Image: "postgres"}
	withVol := base
	withVol.Volumes = []types.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}

	if specHash(base) == specHash(withVol) {
		t.Fatal("adding a volume should change the spec hash (it didn't), so a roll wouldn't trigger")
	}

	// Scaling replicas must NOT change the hash (no roll on scale).
	scaled := withVol
	scaled.Replicas = 1
	withVol.Replicas = 0
	if specHash(scaled) != specHash(withVol) {
		t.Fatal("replicas should not affect the spec hash")
	}
}
