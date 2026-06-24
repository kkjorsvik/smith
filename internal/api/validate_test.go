package api

import (
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestValidateWorkload(t *testing.T) {
	tests := []struct {
		name    string
		wl      types.Workload
		wantErr bool
	}{
		{
			name: "stateless workload, any replicas",
			wl:   types.Workload{ID: "web", Image: "nginx", Replicas: 3},
		},
		{
			name: "stateful, single replica ok",
			wl: types.Workload{ID: "pg", Image: "postgres", Replicas: 1,
				Volumes: []types.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}},
		},
		{
			name: "stateful, replicas defaulted (0) ok",
			wl: types.Workload{ID: "pg", Image: "postgres",
				Volumes: []types.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}},
		},
		{
			name: "stateful, replicas>1 rejected",
			wl: types.Workload{ID: "pg", Image: "postgres", Replicas: 2,
				Volumes: []types.Volume{{Name: "data", Path: "/data"}}},
			wantErr: true,
		},
		{
			name: "bad volume name",
			wl: types.Workload{ID: "pg", Image: "postgres",
				Volumes: []types.Volume{{Name: "Data Dir", Path: "/data"}}},
			wantErr: true,
		},
		{
			name: "relative path rejected",
			wl: types.Workload{ID: "pg", Image: "postgres",
				Volumes: []types.Volume{{Name: "data", Path: "data"}}},
			wantErr: true,
		},
		{
			name: "duplicate volume name rejected",
			wl: types.Workload{ID: "pg", Image: "postgres",
				Volumes: []types.Volume{{Name: "data", Path: "/a"}, {Name: "data", Path: "/b"}}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWorkload(tt.wl)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateWorkload() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
