package health

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/types"
)

// Status represents the health of a single container.
type Status struct {
	Healthy  bool
	Failures int
}

// Monitor runs health checks for all workloads that define them.
// It maintains a map of container ID -> Status that the reconciler
// consults on each tick.
type Monitor struct {
	client    *runtime.Client
	mu        sync.RWMutex
	status    map[string]*Status
	cancels   map[string]context.CancelFunc
	OnHealthy func(id string)
}

// NewMonitor returns a Monitor ready to use.
func NewMonitor(client *runtime.Client) *Monitor {
	return &Monitor{
		client:  client,
		status:  make(map[string]*Status),
		cancels: make(map[string]context.CancelFunc),
	}
}

// Watch starts health checking for a workload.
// If the workload has no health check defined, Watch is a no-op.
// Safe to call multiple times for the same ID — subsequent calls
// are ignored if a watcher is already running.
func (m *Monitor) Watch(w types.Workload) {
	if w.HealthCheck == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Already watching this workload.
	if _, exists := m.cancels[w.ID]; exists {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[w.ID] = cancel
	m.status[w.ID] = &Status{Healthy: true}

	go m.run(ctx, w)
}

// Unwatch stops health checking for a workload and removes its status.
func (m *Monitor) Unwatch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cancel, exists := m.cancels[id]; exists {
		cancel()
		delete(m.cancels, id)
		delete(m.status, id)
	}
}

// Healthy returns true if the container is healthy or has no health check.
func (m *Monitor) Healthy(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, exists := m.status[id]
	if !exists {
		// No health check defined — assume healthy.
		return true
	}
	return s.Healthy
}

// run is the probe loop for a single workload.
func (m *Monitor) run(ctx context.Context, w types.Workload) {
	hc := w.HealthCheck

	log.Printf("health: watching %s (type=%s)", w.ID, hc.Type)

	// Wait initial delay before first probe.
	select {
	case <-time.After(hc.InitialDelay.Duration):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(hc.Interval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := m.probe(w)

			m.mu.Lock()
			s := m.status[w.ID]
			if err != nil {
				s.Failures++
				log.Printf("health: %s probe failed (%d/%d): %v",
					w.ID, s.Failures, hc.Threshold, err)
				if s.Failures >= hc.Threshold {
					s.Healthy = false
					log.Printf("health: %s is unhealthy", w.ID)
					m.mu.Unlock()
					return
				}
			} else {
				if s.Failures > 0 {
					log.Printf("health: %s recovered", w.ID)
					if m.OnHealthy != nil {
						m.OnHealthy(w.ID)
					}
				}
				s.Failures = 0
				s.Healthy = true
			}
			m.mu.Unlock()

		case <-ctx.Done():
			log.Printf("health: stopped watching %s", w.ID)
			return
		}
	}
}

// probe runs a single health check and returns an error if it fails.
func (m *Monitor) probe(w types.Workload) error {
	hc := w.HealthCheck

	switch hc.Type {
	case "http":
		return probeHTTP(hc.URL)
	case "exec":
		return m.probeExec(w.ID, hc.Command)
	default:
		return fmt.Errorf("unknown probe type: %s", hc.Type)
	}
}

// probeHTTP hits a URL and returns an error if the response is not 2xx.
func probeHTTP(url string) error {
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("http probe: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http probe: status %d", resp.StatusCode)
	}

	return nil
}

// probeExec runs a command inside the container and returns an error
// if the exit code is non-zero.
func (m *Monitor) probeExec(id string, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("exec probe: no command specified")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code, err := m.client.ExecInContainer(ctx, id, command)
	if err != nil {
		return fmt.Errorf("exec probe: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("exec probe: exit code %d", code)
	}

	return nil
}
