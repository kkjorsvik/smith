package runtime

import (
	"context"
	"fmt"
	"log"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/kkjorsvik/smith/internal/types"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	SmithNamespace = "smith"
	SocketPath     = defaults.DefaultAddress
)

// Client wraps the containerd client with smith-specific defaults.
type Client struct {
	inner *containerd.Client
}

// NewClient opens a connection to containerd and returns a Client.
// The caller is responsible for calling Close().
func NewClient() (*Client, error) {
	c, err := containerd.New(SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", SocketPath, err)
	}
	return &Client{inner: c}, nil
}

// Close releases the containerd connection.
func (c *Client) Close() error {
	return c.inner.Close()
}

// Context returns a context pre-loaded with the smith namespace.
// Every containerd API call needs this — always use this instead of
// a bare context.Background().
func (c *Client) Context() context.Context {
	return namespaces.WithNamespace(context.Background(), SmithNamespace)
}

// PullImage pulls an OCI image from a registry and unpacks it into
// the snapshotter. ref should be a fully qualified image reference,
// e.g. "docker.io/library/alpine:3.19".
func (c *Client) PullImage(ref string) (containerd.Image, error) {
	ctx := c.Context()

	image, err := c.inner.Pull(ctx, ref,
		containerd.WithPullUnpack,
		containerd.WithResolver(docker.NewResolver(docker.ResolverOptions{})),
	)
	if err != nil {
		return nil, fmt.Errorf("pull image %s: %w", ref, err)
	}

	log.Printf("pulled image: %s", ref)
	return image, nil
}

// GetImage returns a local image if it exists, without hitting the registry.
func (c *Client) GetImage(ref string) (containerd.Image, error) {
	ctx := c.Context()
	image, err := c.inner.GetImage(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("get image %s: %w", ref, err)
	}
	return image, nil
}

// RunOptions configures how a container is run.
type RunOptions struct {
	// ID is the unique container identifier within the smith namespace.
	ID string
	// Image is a previously pulled containerd image.
	Image containerd.Image
	// Args overrides the image's default entrypoint/cmd.
	// If nil, the image's default is used.
	Args []string
	// Started is closed once the task is confirmed running.
	// Optional — callers that need to know when the task is live
	// should pass a make(chan struct{}) and block on it.
	Started chan struct{}
	// Ports are host->container port mappings to publish via CNI's
	// portmap plugin. Only applied when CNI is non-nil.
	Ports []types.PortMapping
	// CNI, when non-nil, configures the container's network namespace
	// (bridge IP + port mappings). When nil, networking is left as
	// containerd's default and Ports is ignored.
	CNI *CNI
	// Env are environment variables (KEY=VALUE) injected into the
	// container, merged over the image's defaults.
	Env map[string]string
}

// RunContainer creates a container, starts it, waits for it to exit,
// then cleans up the container and its snapshot.
//
// Returns the exit code and any error. If the container ID or snapshot
// already exists, the error wraps errdefs.ErrAlreadyExists — check with
// ErrAlreadyExists().
func (c *Client) RunContainer(opts RunOptions) (uint32, error) {
	ctx := c.Context()

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(opts.Image),
	}
	if len(opts.Args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(opts.Args...))
	}
	if len(opts.Env) > 0 {
		envSlice := make([]string, 0, len(opts.Env))
		for k, v := range opts.Env {
			envSlice = append(envSlice, fmt.Sprintf("%s=%s", k, v))
		}
		specOpts = append(specOpts, oci.WithEnv(envSlice))
	}

	// NewContainer creates the metadata record and a fresh writable
	// snapshot. It does NOT start a process.
	container, err := c.inner.NewContainer(ctx,
		opts.ID,
		containerd.WithImage(opts.Image),
		containerd.WithNewSnapshot(opts.ID+"-snapshot", opts.Image),
		containerd.WithNewSpec(specOpts...),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return 0, fmt.Errorf("create container %s: %w", opts.ID, errdefs.ErrAlreadyExists)
		}
		return 0, fmt.Errorf("create container %s: %w", opts.ID, err)
	}
	// Track whether the container started successfully. On any early
	// error return, clean up the container and snapshot. StopContainer
	// may also delete this container out from under us when the workload
	// is unassigned, so the not-found case is tolerated.
	started := false
	defer func() {
		if !started {
			if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
				if !errdefs.IsNotFound(err) {
					log.Printf("warn: container cleanup %s: %v", opts.ID, err)
				}
			}
		}
	}()

	// NewTask creates the actual OS process from the container spec.
	// cio.WithStdio wires the container's stdin/stdout/stderr to ours.
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		return 0, fmt.Errorf("create task for %s: %w", opts.ID, err)
	}

	// Clean up the task on any early error return. This defer is registered
	// after the container-cleanup defer, so it runs FIRST (LIFO): the task
	// is killed and deleted before the container delete, avoiding the
	// "cannot delete running task" precondition error that leaves a ghost.
	taskCleanup := true
	defer func() {
		if taskCleanup {
			if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil {
				if !errdefs.IsNotFound(err) {
					log.Printf("warn: task cleanup %s: %v", opts.ID, err)
				}
			}
		}
	}()

	// Set up CNI networking if configured. The task already has its own
	// network namespace at /proc/<pid>/ns/net; CNI populates it with a
	// bridge IP and any host port mappings before the process starts.
	var netnsPath string
	if opts.CNI != nil {
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", task.Pid())
		if _, err := opts.CNI.Setup(ctx, opts.ID, netnsPath, opts.Ports); err != nil {
			log.Printf("RunContainer %s FAILED at cni-setup: %v", opts.ID, err)
			return 0, fmt.Errorf("cni setup for %s: %w", opts.ID, err)
		}
		log.Printf("RunContainer %s: CNI setup OK, netns=%s", opts.ID, netnsPath)
	}

	// Tear down CNI on any early error return after setup succeeded. Runs
	// before the task-cleanup defer (LIFO), matching the correct teardown
	// order: CNI first, then task, then container.
	cniDone := false
	defer func() {
		if !cniDone && opts.CNI != nil && netnsPath != "" {
			log.Printf("RunContainer %s: cleanup CNI teardown attempt netns=%s", opts.ID, netnsPath)
			if err := opts.CNI.Teardown(ctx, opts.ID, netnsPath, opts.Ports); err != nil {
				log.Printf("RunContainer %s: cleanup CNI teardown FAILED: %v", opts.ID, err)
			} else {
				log.Printf("RunContainer %s: cleanup CNI teardown SUCCESS", opts.ID)
			}
		}
	}()

	// Call Wait BEFORE Start — if the process exits fast you can miss
	// the exit event if Wait is called after Start.
	exitCh, err := task.Wait(ctx)
	if err != nil {
		log.Printf("RunContainer %s FAILED at task-wait: %v", opts.ID, err)
		return 0, fmt.Errorf("wait setup for %s: %w", opts.ID, err)
	}

	log.Printf("RunContainer %s: starting task", opts.ID)
	if err := task.Start(ctx); err != nil {
		log.Printf("RunContainer %s FAILED at task-start: %v", opts.ID, err)
		return 0, fmt.Errorf("start task %s: %w", opts.ID, err)
	}

	if opts.Started != nil {
		close(opts.Started)
	}
	log.Printf("task started: %s (pid %d)", opts.ID, task.Pid())

	// From here the container is successfully running. Disable the
	// early-return container cleanup defer; teardown is handled explicitly
	// below in the correct order once the process exits.
	started = true

	// Block until the container process exits.
	status := <-exitCh

	// Explicit teardown on normal exit, in the correct order: CNI, then
	// task, then container. Each flag is cleared so the matching defer does
	// not double-delete. not-found is tolerated because StopContainer may
	// have torn part of this down concurrently on an unassign.
	if opts.CNI != nil && netnsPath != "" {
		if err := opts.CNI.Teardown(ctx, opts.ID, netnsPath, opts.Ports); err != nil {
			log.Printf("warn: cni teardown %s: %v", opts.ID, err)
		}
	}
	cniDone = true

	// Delete the task AFTER receiving exit status — deferring this
	// races with exit status collection and can swallow signal codes.
	if _, err := task.Delete(ctx); err != nil {
		if !errdefs.IsNotFound(err) {
			log.Printf("warn: task delete %s: %v", opts.ID, err)
		}
	}
	taskCleanup = false

	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		if !errdefs.IsNotFound(err) {
			log.Printf("warn: container delete %s: %v", opts.ID, err)
		}
	}

	code, _, err := status.Result()
	if err != nil {
		return 0, fmt.Errorf("exit status for %s: %w", opts.ID, err)
	}

	log.Printf("task exited: %s (code %d)", opts.ID, code)
	return code, nil
}

// KillContainer sends a signal to a running container's task.
// Use force=true to send SIGKILL, false to send SIGTERM.
// In a real orchestrator you'd send SIGTERM first, wait a grace period,
// then follow up with SIGKILL if still running.
func (c *Client) KillContainer(id string, force bool) error {
	ctx := c.Context()

	container, err := c.inner.LoadContainer(ctx, id)
	if err != nil {
		return fmt.Errorf("load container %s: %w", id, err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("get task %s: %w", id, err)
	}

	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}

	if err := task.Kill(ctx, sig); err != nil {
		return fmt.Errorf("kill task %s: %w", id, err)
	}

	return nil
}

// StopContainer kills a container's task, tears down its CNI networking,
// and removes the task, container, and snapshot. This is the full cleanup
// path used when a workload is unassigned. It is idempotent — missing
// containers or tasks are treated as already cleaned up.
func (c *Client) StopContainer(id string, cni *CNI, ports []types.PortMapping) error {
	ctx := c.Context()

	container, err := c.inner.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("load container %s: %w", id, err)
	}

	task, err := container.Task(ctx, nil)
	if err == nil {
		// Capture netns path before the task is gone, for CNI teardown.
		netnsPath := fmt.Sprintf("/proc/%d/ns/net", task.Pid())

		// Kill and wait for exit.
		exitCh, waitErr := task.Wait(ctx)
		if waitErr == nil {
			task.Kill(ctx, syscall.SIGKILL)
			select {
			case <-exitCh:
			case <-time.After(10 * time.Second):
				log.Printf("warn: timeout waiting for %s to exit", id)
			}
		}

		// Tear down CNI networking before deleting the task.
		if cni != nil {
			if err := cni.Teardown(ctx, id, netnsPath, ports); err != nil {
				log.Printf("warn: cni teardown %s: %v", id, err)
			}
		}

		if _, err := task.Delete(ctx); err != nil {
			log.Printf("warn: task delete %s: %v", id, err)
		}
	}

	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("delete container %s: %w", id, err)
		}
	}

	log.Printf("stopped and cleaned up container %s", id)
	return nil
}

// ErrAlreadyExists returns true if the error indicates a resource
// already exists in containerd. Use this in the reconciler to
// distinguish "already running" from actual failures.
func ErrAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return errdefs.IsAlreadyExists(err)
}

// ContainerStatus represents the observed state of a single container.
type ContainerStatus struct {
	ID     string                   `json:"id"`
	Status containerd.ProcessStatus `json:"status"`
	Pid    uint32                   `json:"pid"`
}

// ListRunning returns all containers in the smith namespace and their
// current task status. Containers with no task (created but never started,
// or already cleaned up) are included with status Unknown.
func (c *Client) ListRunning() (map[string]ContainerStatus, error) {
	ctx := c.Context()

	containers, err := c.inner.Containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make(map[string]ContainerStatus, len(containers))

	for _, container := range containers {
		status := ContainerStatus{
			ID:     container.ID(),
			Status: containerd.Unknown,
		}

		// A container may exist without a task if it was created but
		// never started, or if the task already exited and was deleted.
		task, err := container.Task(ctx, nil)
		if err == nil {
			state, err := task.Status(ctx)
			if err == nil {
				status.Status = state.Status
				status.Pid = task.Pid()
			}
		}

		out[container.ID()] = status
	}

	return out, nil
}

// Cleanup force-removes a container and its snapshot regardless of
// task state. Used on startup to clear ghost containers left by a
// previous unclean shutdown.
func (c *Client) Cleanup(id string) error {
	ctx := c.Context()

	container, err := c.inner.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("load container %s: %w", id, err)
	}

	// If a task exists, wait for it to exit after killing it.
	task, err := container.Task(ctx, nil)
	if err == nil {
		exitCh, err := task.Wait(ctx)
		if err == nil {
			task.Kill(ctx, syscall.SIGKILL)
			<-exitCh
		}
		task.Delete(ctx)
	}

	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("delete container %s: %w", id, err)
	}

	return nil
}

// CleanupAll removes all containers in the smith namespace.
// Call this on startup before the reconciler starts.
func (c *Client) CleanupAll() error {
	ctx := c.Context()

	containers, err := c.inner.Containers(ctx)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	for _, container := range containers {
		if err := c.Cleanup(container.ID()); err != nil {
			log.Printf("cleanup %s: %v", container.ID(), err)
		} else {
			log.Printf("cleanup: removed ghost container %s", container.ID())
		}
	}

	return nil
}

// ExecInContainer runs a command inside an existing container's task
// and returns the exit code. Used by health check exec probes.
func (c *Client) ExecInContainer(ctx context.Context, id string, command []string) (uint32, error) {
	ctx = namespaces.WithNamespace(ctx, SmithNamespace)

	container, err := c.inner.LoadContainer(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("load container %s: %w", id, err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("get task %s: %w", id, err)
	}

	// Generate a unique exec ID so multiple probes can run concurrently
	// without colliding.
	execID := fmt.Sprintf("health-%s-%d", id, time.Now().UnixNano())

	spec := &specs.Process{
		Args: command,
		Cwd:  "/",
		Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}

	process, err := task.Exec(ctx, execID, spec, cio.NullIO)
	if err != nil {
		return 0, fmt.Errorf("exec in %s: %w", id, err)
	}
	defer process.Delete(ctx)

	exitCh, err := process.Wait(ctx)
	if err != nil {
		return 0, fmt.Errorf("wait exec in %s: %w", id, err)
	}

	if err := process.Start(ctx); err != nil {
		return 0, fmt.Errorf("start exec in %s: %w", id, err)
	}

	status := <-exitCh
	code, _, err := status.Result()
	if err != nil {
		return 0, fmt.Errorf("exec exit status %s: %w", id, err)
	}

	return code, nil
}
