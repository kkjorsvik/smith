package registry

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kkjorsvik/smith/internal/types"
)

const (
	// HeartbeatTimeout is how long before a node is considered dead.
	HeartbeatTimeout = 30 * time.Second
)

// Registry tracks all worker nodes known to the control plane.
// It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]types.Node
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		nodes: make(map[string]types.Node),
	}
}

// Register adds or updates a node in the registry.
func (r *Registry) Register(n types.Node) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n.LastHeartbeat = time.Now()
	r.nodes[n.ID] = n
	log.Printf("registry: registered node %s at %s", n.ID, n.Addr)
}

// Heartbeat updates the last heartbeat time for a node.
// Returns an error if the node is not registered.
func (r *Registry) Heartbeat(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, exists := r.nodes[id]
	if !exists {
		return fmt.Errorf("node %s not registered", id)
	}

	n.LastHeartbeat = time.Now()
	r.nodes[id] = n
	return nil
}

// Get returns a single node by ID.
func (r *Registry) Get(id string) (types.Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n, exists := r.nodes[id]
	return n, exists
}

// List returns all currently registered nodes.
func (r *Registry) List() []types.Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]types.Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n)
	}
	return out
}

// Alive returns all nodes that have sent a heartbeat within HeartbeatTimeout.
func (r *Registry) Alive() []types.Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cutoff := time.Now().Add(-HeartbeatTimeout)
	out := make([]types.Node, 0)
	for _, n := range r.nodes {
		if n.LastHeartbeat.After(cutoff) {
			out = append(out, n)
		}
	}
	return out
}

// Dead returns all nodes that have missed their heartbeat window.
func (r *Registry) Dead() []types.Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cutoff := time.Now().Add(-HeartbeatTimeout)
	out := make([]types.Node, 0)
	for _, n := range r.nodes {
		if !n.LastHeartbeat.After(cutoff) {
			out = append(out, n)
		}
	}
	return out
}

// Remove deletes a node from the registry.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.nodes, id)
	log.Printf("registry: removed node %s", id)
}
