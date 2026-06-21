package reconciler

import (
	"log"
	"sync"
	"time"

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
// converge them: starting missing containers and stopping extra ones.
type Reconciler struct {
	client   *runtime.Client
	store    Storer
	monitor  *health.Monitor
	interval time.Duration

	mu      sync.Mutex
	running map[string]bool

	stop chan struct{}
	done chan struct{}
}

// New returns a Reconciler that reconciles every interval.
func New(client *runtime.Client, store Storer, monitor *health.Monitor, interval time.Duration) *Reconciler {
	return &Reconciler{
		client:   client,
		store:    store,
		monitor:  monitor,
		interval: interval,
		running:  make(map[string]bool),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the reconcile loop in a goroutine.
func (r *Reconciler) Start() {
	go r.loop()
}

// Stop halts the reconcile loop and tears down every running container.
func (r *Reconciler) Stop() {
	close(r.stop)
	<-r.done

	r.mu.Lock()
	defer r.mu.Unlock()

	for id := range r.running {
		if err := r.client.KillContainer(id, true); err != nil {
			log.Printf("reconciler: stop %s: %v", id, err)
		}
		r.monitor.Unwatch(id)
		delete(r.running, id)
	}
}

func (r *Reconciler) loop() {
	defer close(r.done)

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

	r.mu.Lock()
	defer r.mu.Unlock()

	// Start workloads that are desired but not yet running.
	for id, w := range desired {
		if r.running[id] {
			continue
		}
		r.launch(w)
	}

	// Stop workloads that are running but no longer desired.
	for id := range r.running {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := r.client.KillContainer(id, true); err != nil {
			log.Printf("reconciler: kill %s: %v", id, err)
		}
		r.monitor.Unwatch(id)
		delete(r.running, id)
	}
}

// launch pulls the image and starts the container in a background
// goroutine. Must be called with r.mu held.
func (r *Reconciler) launch(w types.Workload) {
	image, err := r.client.PullImage(w.Image)
	if err != nil {
		log.Printf("reconciler: pull image for %s: %v", w.ID, err)
		return
	}

	r.running[w.ID] = true
	r.monitor.Watch(w)

	go func() {
		_, err := r.client.RunContainer(runtime.RunOptions{
			ID:    w.ID,
			Image: image,
			Args:  w.Args,
		})
		if err != nil && !runtime.ErrAlreadyExists(err) {
			log.Printf("reconciler: run %s exited: %v", w.ID, err)
		}

		// The container has exited; drop it from the running set so the
		// next reconcile tick can restart it if it is still desired.
		r.mu.Lock()
		delete(r.running, w.ID)
		r.mu.Unlock()
	}()
}
