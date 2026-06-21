package reconciler

import (
	"log"
	"sync"
	"time"

	"github.com/containerd/containerd"
	"github.com/kkjorsvik/smith/internal/health"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/types"
)

// Storer is the desired-state backend the reconciler drives toward.
// Both the in-memory Store and the SQLiteStore satisfy it.
type Storer interface {
	Add(w types.Workload) error
	Remove(id string) error
	List() (map[string]types.Workload, error)
}

// Reconciler periodically compares the desired state (from the store)
// against the observed state (from containerd) and takes action to
// converge them: starting missing containers, stopping extra ones, and
// restarting unhealthy ones with exponential backoff.
type Reconciler struct {
	client   *runtime.Client
	store    Storer
	monitor  *health.Monitor
	interval time.Duration
	stop     chan struct{}
	failures map[string]int
	mu       sync.Mutex
}

// New returns a Reconciler that reconciles every interval.
func New(client *runtime.Client, store Storer, monitor *health.Monitor, interval time.Duration) *Reconciler {
	return &Reconciler{
		client:   client,
		store:    store,
		monitor:  monitor,
		interval: interval,
		stop:     make(chan struct{}),
		failures: make(map[string]int),
	}
}

// Start launches the reconcile loop in a goroutine.
func (r *Reconciler) Start() {
	go r.loop()
}

// ResetFailures resets the restart counter for a workload.
// Called by the health monitor when a container recovers.
func (r *Reconciler) ResetFailures(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, id)
}

// Stop halts the reconcile loop. Containers left running are cleaned up by
// CleanupAll on the next startup.
func (r *Reconciler) Stop() {
	close(r.stop)
}

func (r *Reconciler) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.reconcile()
	for {
		select {
		case <-ticker.C:
			r.reconcile()
		case <-r.stop:
			return
		}
	}
}

// reconcile drives observed state toward desired state for one tick.
func (r *Reconciler) reconcile() {
	desired, err := r.store.List()
	if err != nil {
		log.Printf("reconciler: list desired state: %v", err)
		return
	}

	observed, err := r.client.ListRunning()
	if err != nil {
		log.Printf("reconciler: list running: %v", err)
		return
	}

	for id, w := range desired {
		obs, running := observed[id]

		// Not running yet — (re)start it. start() arms the health monitor
		// once the task is confirmed running.
		if !running || obs.Status != containerd.Running {
			log.Printf("reconciler: starting %s", id)
			r.start(w)
			continue
		}

		if obs.Status == containerd.Running && !r.monitor.Healthy(id) {
			r.mu.Lock()
			r.failures[id]++
			attempt := r.failures[id]
			r.mu.Unlock()

			backoff := time.Duration(attempt) * 5 * time.Second
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}

			log.Printf("reconciler: %s is unhealthy, restarting in %s (attempt %d)", id, backoff, attempt)

			go func(containerID string, delay time.Duration) {
				time.Sleep(delay)
				if err := r.client.KillContainer(containerID, true); err != nil {
					log.Printf("reconciler: kill unhealthy %s: %v", containerID, err)
				}
			}(id, backoff)
		}
	}

	// Stop containers that are running but no longer desired.
	for id := range observed {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := r.client.KillContainer(id, true); err != nil {
			log.Printf("reconciler: kill %s: %v", id, err)
		}
		r.monitor.Unwatch(id)
	}
}

func (r *Reconciler) start(w types.Workload) {
	go func() {
		image, err := r.client.GetImage(w.Image)
		if err != nil {
			image, err = r.client.PullImage(w.Image)
			if err != nil {
				log.Printf("reconciler: pull %s: %v", w.ID, err)
				return
			}
		}

		started := make(chan struct{})
		abort := make(chan struct{})

		go func() {
			select {
			case <-started:
				r.ResetFailures(w.ID)
				r.monitor.Watch(w)
			case <-abort:
				// Container failed to start — nothing to watch.
			}
		}()

		code, err := r.client.RunContainer(runtime.RunOptions{
			ID:      w.ID,
			Image:   image,
			Args:    w.Args,
			Started: started,
		})
		if err != nil {
			close(abort)
			if runtime.ErrAlreadyExists(err) {
				return
			}
			log.Printf("reconciler: run %s: %v", w.ID, err)
			return
		}

		r.monitor.Unwatch(w.ID)
		log.Printf("reconciler: %s exited (code %d)", w.ID, code)
	}()
}
