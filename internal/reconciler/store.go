package reconciler

import (
	"fmt"
	"sync"

	"github.com/kkjorsvik/smith/internal/types"
)

// Store is an in-memory implementation of Storer. It keeps the desired
// state of all workloads in a map guarded by a read/write mutex.
type Store struct {
	mu        sync.RWMutex
	workloads map[string]types.Workload
}

func NewStore() *Store {
	return &Store{
		workloads: make(map[string]types.Workload),
	}
}

func (s *Store) Add(w types.Workload) error {
	if w.ID == "" {
		return fmt.Errorf("workload ID cannot be empty")
	}
	if w.Image == "" {
		return fmt.Errorf("workload %s: image cannot be empty", w.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.workloads[w.ID] = w
	return nil
}

// Remove deletes a workload from the desired state by ID.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.workloads, id)
	return nil
}

// List returns a snapshot of all desired workloads.
func (s *Store) List() (map[string]types.Workload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]types.Workload, len(s.workloads))
	for k, v := range s.workloads {
		out[k] = v
	}
	return out, nil
}
