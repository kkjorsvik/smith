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
	defer container.Delete(ctx, containerd.WithSnapshotCleanup)

	// NewTask creates the actual OS process from the container spec.
	// cio.WithStdio wires the container's stdin/stdout/stderr to ours.
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		return 0, fmt.Errorf("create task for %s: %w", opts.ID, err)
	}

	// Call Wait BEFORE Start — if the process exits fast you can miss
	// the exit event if Wait is called after Start.
	exitCh, err := task.Wait(ctx)
	if err != nil {
		return 0, fmt.Errorf("wait setup for %s: %w", opts.ID, err)
	}

	if err := task.Start(ctx); err != nil {
		return 0, fmt.Errorf("start task %s: %w", opts.ID, err)
	}

	if opts.Started != nil {
		close(opts.Started)
	}
	log.Printf("task started: %s (pid %d)", opts.ID, task.Pid())

	// Block until the container process exits.
	status := <-exitCh

	// Delete the task AFTER receiving exit status — deferring this
	// races with exit status collection and can swallow signal codes.
	if _, err := task.Delete(ctx); err != nil {
		log.Printf("warn: task delete %s: %v", opts.ID, err)
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
	ID     string
	Status containerd.ProcessStatus
	Pid    uint32
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
