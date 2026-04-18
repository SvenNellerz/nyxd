// Package supervisor manages the full container lifecycle.
package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/zrougamed/nyxd/internal/compose"
	"github.com/zrougamed/nyxd/internal/image"
	"github.com/zrougamed/nyxd/internal/logs"
	"github.com/zrougamed/nyxd/internal/network"
	"github.com/zrougamed/nyxd/internal/overlay"
	"github.com/zrougamed/nyxd/internal/runtime"
	"github.com/zrougamed/nyxd/internal/telemetry"
)

// ContainerStatus represents a container's lifecycle phase.
type ContainerStatus int

const (
	StatusCreated ContainerStatus = iota
	StatusPulling
	StatusStarting
	StatusRunning
	StatusHealthy
	StatusUnhealthy
	StatusStopping
	StatusStopped
	StatusFailed
)

func (s ContainerStatus) String() string {
	return [...]string{
		"created", "pulling", "starting", "running",
		"healthy", "unhealthy", "stopping", "stopped", "failed",
	}[s]
}

// Container manages a single container's lifecycle.
type container struct {
	name string
	svc  compose.Service
	cfg  Config

	// subsystems
	runner  *runtime.Runner
	overlay *overlay.Manager
	netMgr  *network.Manager
	puller  *image.Puller
	unpack  *image.Unpacker
	tel     *telemetry.Telemetry

	// state
	mu         sync.Mutex
	status     ContainerStatus
	restarts   int
	netNSPath  string
	bundleDir  string
	overlayMnt *overlay.Mount
	logger     *logs.Logger
	stopOnce   sync.Once
	stopCh     chan struct{} // closed when container should stop
	exitCh     chan error    // receives the wait result when process exits
}

// newContainer creates a container manager for a service.
func newContainer(name string, svc compose.Service, cfg Config) (*container, error) {
	runner, err := runtime.NewRunner(cfg.CrunPath, filepath.Join(cfg.StateDir, "crun"))
	if err != nil {
		return nil, err
	}

	puller := image.NewPuller(filepath.Join(cfg.StateDir, "images"), "linux/amd64")
	unpack := image.NewUnpacker(puller, filepath.Join(cfg.StateDir, "layers"))
	ovl := overlay.NewManager(filepath.Join(cfg.StateDir, "overlay"))
	netMgr := network.NewManager(cfg.CNIConfDir, cfg.CNIPath)

	return &container{
		name:    name,
		svc:     svc,
		cfg:     cfg,
		runner:  runner,
		overlay: ovl,
		netMgr:  netMgr,
		puller:  puller,
		unpack:  unpack,
		tel:     cfg.Telemetry,
		status:  StatusCreated,
		stopCh:  make(chan struct{}),
		exitCh:  make(chan error, 1),
	}, nil
}

// run is the main goroutine for this container. Handles start, restart loop, and shutdown.
func (c *container) run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("container goroutine panicked", "container", c.name, "panic", r)
		}
	}()

	for {
		err := c.startOnce(ctx)
		if err != nil {
			c.setStatus(StatusFailed)
			c.tel.ContainersCrashed.Add(1)
			c.tel.Event(c.name, "crash", "error", err)
			slog.Error("container exited with error", "container", c.name, "error", err)
		} else {
			c.setStatus(StatusStopped)
			c.tel.Event(c.name, "stopped")
		}

		// Check if we should restart
		if !c.shouldRestart(err) {
			slog.Info("container will not restart", "container", c.name, "policy", c.svc.Restart)
			return
		}

		// Check if context is done (shutdown signal)
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		default:
		}

		c.mu.Lock()
		c.restarts++
		restarts := c.restarts
		c.mu.Unlock()

		c.tel.ContainersRestarted.Add(1)
		c.tel.Event(c.name, "restart", "count", restarts)
		slog.Info("restarting container", "container", c.name, "attempt", restarts)

		// Backoff: min(30s, 2^restarts * 100ms)
		backoff := backoffDuration(restarts)
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-time.After(backoff):
		}
	}
}

// startOnce performs one full container lifecycle: pull → unpack → mount → run → wait.
func (c *container) startOnce(ctx context.Context) error {
	// --- Pull image ---
	c.setStatus(StatusPulling)
	digest, err := c.puller.Pull(ctx, c.svc.Image)
	if err != nil {
		return fmt.Errorf("pull %s: %w", c.svc.Image, err)
	}
	c.tel.ImagesPulled.Add(1)

	// --- Unpack layers ---
	unpackResult, err := c.unpack.Unpack(digest)
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}

	// --- Overlayfs ---
	c.setStatus(StatusStarting)
	mnt, err := c.overlay.Prepare(c.name, unpackResult.Layers)
	if err != nil {
		return fmt.Errorf("overlayfs: %w", err)
	}
	c.mu.Lock()
	c.overlayMnt = mnt
	c.mu.Unlock()

	// --- Network namespace ---
	netNS, err := network.CreateNetNS(c.name)
	if err != nil {
		_ = c.overlay.Teardown(c.name)
		return fmt.Errorf("create netns: %w", err)
	}
	c.mu.Lock()
	c.netNSPath = netNS
	c.mu.Unlock()

	// --- CNI ---
	var ips []string
	if len(c.svc.Networks) > 0 {
		addrs, err := c.netMgr.Setup(ctx, c.name, netNS, c.svc.Networks)
		if err != nil {
			_ = network.DeleteNetNS(c.name)
			_ = c.overlay.Teardown(c.name)
			return fmt.Errorf("CNI setup: %w", err)
		}
		for _, ip := range addrs {
			ips = append(ips, ip.String())
		}
	}

	// --- OCI spec ---
	bundleDir := filepath.Join(c.cfg.StateDir, "bundles", c.name)
	spec, err := runtime.BuildSpec(c.name, c.svc, unpackResult.Config, mnt.MergedDir, netNS, ips)
	if err != nil {
		c.cleanup(ctx)
		return fmt.Errorf("build spec: %w", err)
	}
	if err := runtime.WriteSpec(bundleDir, spec); err != nil {
		c.cleanup(ctx)
		return fmt.Errorf("write spec: %w", err)
	}
	c.mu.Lock()
	c.bundleDir = bundleDir
	c.mu.Unlock()

	// --- Logger ---
	logDir := filepath.Join(c.cfg.StateDir, "logs")
	logger, err := logs.New(logDir, c.name, true)
	if err != nil {
		c.cleanup(ctx)
		return fmt.Errorf("logger: %w", err)
	}
	c.mu.Lock()
	c.logger = logger
	c.mu.Unlock()

	// --- crun create ---
	if err := c.runner.Create(ctx, c.name, bundleDir); err != nil {
		logger.Close()
		c.cleanup(ctx)
		return fmt.Errorf("crun create: %w", err)
	}

	// --- Attach log pipes ---
	// crun writes container stdout/stderr to /proc/<pid>/fd/1 and /proc/<pid>/fd/2.
	// We use a pipe pair attached via crun's --console-socket or /dev/pts.
	// For simplicity in this implementation, we use a named pipe approach:
	stdoutPath := filepath.Join(bundleDir, "stdout.pipe")
	stderrPath := filepath.Join(bundleDir, "stderr.pipe")
	stdoutR, stderrR, err := attachLogPipes(stdoutPath, stderrPath)
	if err != nil {
		_ = c.runner.Delete(ctx, c.name)
		logger.Close()
		c.cleanup(ctx)
		return fmt.Errorf("attach log pipes: %w", err)
	}

	logCtx, logCancel := context.WithCancel(ctx)
	defer logCancel()

	go logger.StreamReader(logCtx, stdoutR, "stdout")
	go logger.StreamReader(logCtx, stderrR, "stderr")

	// --- crun start ---
	if err := c.runner.Start(ctx, c.name); err != nil {
		logCancel()
		_ = c.runner.Delete(ctx, c.name)
		logger.Close()
		c.cleanup(ctx)
		return fmt.Errorf("crun start: %w", err)
	}

	c.setStatus(StatusRunning)
	c.tel.ContainersStarted.Add(1)
	c.tel.Event(c.name, "started", "image", c.svc.Image, "ips", ips)

	// --- Healthcheck ---
	var hcCancel context.CancelFunc
	var hcCtx context.Context
	if c.svc.Healthcheck != nil && c.svc.Healthcheck.Test[0] != "NONE" {
		hcCtx, hcCancel = context.WithCancel(ctx)
		go c.runHealthcheck(hcCtx)
	}

	// --- Wait for exit ---
	waitErr := c.waitForExit(ctx)

	// Cleanup in order
	if hcCancel != nil {
		hcCancel()
	}
	logCancel()
	logger.Close()
	stdoutR.Close()
	stderrR.Close()

	_ = c.runner.Delete(context.Background(), c.name)
	c.cleanup(context.Background())

	return waitErr
}

// waitForExit polls crun state until the container exits or ctx is cancelled.
func (c *container) waitForExit(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.stop(context.Background())
			return ctx.Err()
		case <-c.stopCh:
			c.stop(context.Background())
			return nil
		case <-ticker.C:
			st, err := c.runner.State(ctx, c.name)
			if err != nil {
				return fmt.Errorf("state poll: %w", err)
			}
			if st == nil || st.Status == "stopped" {
				return nil
			}
		}
	}
}

// stop sends SIGTERM → SIGKILL to the container.
func (c *container) stop(ctx context.Context) {
	c.setStatus(StatusStopping)
	timeout := c.svc.StopTimeout.Duration
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if err := c.runner.GracefulStop(ctx, c.name, timeout); err != nil {
		slog.Warn("graceful stop error", "container", c.name, "error", err)
	}
}

// Signal sends a stop signal to this container's goroutine.
func (c *container) Signal() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// cleanup tears down overlay and network after a container exits.
func (c *container) cleanup(ctx context.Context) {
	c.mu.Lock()
	netNS := c.netNSPath
	c.netNSPath = ""
	c.mu.Unlock()

	if netNS != "" {
		if err := c.netMgr.Teardown(ctx, c.name, netNS); err != nil {
			slog.Warn("CNI teardown error", "container", c.name, "error", err)
		}
		if err := network.DeleteNetNS(c.name); err != nil {
			slog.Warn("netns delete error", "container", c.name, "error", err)
		}
	}

	if err := c.overlay.Teardown(c.name); err != nil {
		slog.Warn("overlay teardown error", "container", c.name, "error", err)
	}

	// Remove bundle dir
	c.mu.Lock()
	bd := c.bundleDir
	c.bundleDir = ""
	c.mu.Unlock()
	if bd != "" {
		_ = os.RemoveAll(bd)
	}
}

// shouldRestart returns true if the container should restart based on policy and error.
func (c *container) shouldRestart(err error) bool {
	select {
	case <-c.stopCh:
		return false
	default:
	}
	switch c.svc.Restart {
	case compose.RestartAlways, compose.RestartUnlessStopped:
		return true
	case compose.RestartOnFailure:
		return err != nil
	default:
		return false
	}
}

func (c *container) setStatus(s ContainerStatus) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
	slog.Info("container status changed", "container", c.name, "status", s.String())
}

// runHealthcheck periodically checks container health.
func (c *container) runHealthcheck(ctx context.Context) {
	hc := c.svc.Healthcheck
	if len(hc.Test) < 2 {
		return
	}

	// Wait for start period
	if hc.StartPeriod.Duration > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(hc.StartPeriod.Duration):
		}
	}

	ticker := time.NewTicker(hc.Interval.Duration)
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := c.execHealthcheck(ctx, hc)
			if err != nil {
				failures++
				c.tel.HealthchecksFailed.Add(1)
				slog.Warn("healthcheck failed",
					"container", c.name,
					"failures", failures,
					"threshold", hc.Retries,
					"error", err,
				)
				if failures >= hc.Retries {
					c.setStatus(StatusUnhealthy)
					c.tel.Event(c.name, "unhealthy", "failures", failures)
				}
			} else {
				if failures > 0 {
					slog.Info("healthcheck recovered", "container", c.name)
					c.tel.Event(c.name, "healthy")
				}
				failures = 0
				c.setStatus(StatusHealthy)
				c.tel.HealthchecksOK.Add(1)
			}
		}
	}
}

func (c *container) execHealthcheck(ctx context.Context, hc *compose.Healthcheck) error {
	hcCtx, cancel := context.WithTimeout(ctx, hc.Timeout.Duration)
	defer cancel()

	switch hc.Test[0] {
	case "CMD":
		return c.runner.Exec(hcCtx, c.name, hc.Test[1:])
	case "CMD-SHELL":
		return c.runner.Exec(hcCtx, c.name, []string{"/bin/sh", "-c", hc.Test[1]})
	default:
		return nil
	}
}

// backoffDuration returns exponential backoff capped at 30s.
func backoffDuration(attempt int) time.Duration {
	ms := 100 * (1 << min(attempt, 8))
	if ms > 30000 {
		ms = 30000
	}
	return time.Duration(ms) * time.Millisecond
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// attachLogPipes creates named pipes for stdout/stderr and returns readers.
// In production, these would be passed to crun via OCI spec stdio hooks.
// This implementation uses FIFO pairs wired through the bundle directory.
func attachLogPipes(stdoutPath, stderrPath string) (io.ReadCloser, io.ReadCloser, error) {
	// Create FIFOs
	for _, p := range []string{stdoutPath, stderrPath} {
		_ = os.Remove(p)
		if err := mkfifo(p, 0o600); err != nil {
			return nil, nil, fmt.Errorf("mkfifo %s: %w", p, err)
		}
	}

	// Open read ends (non-blocking to avoid hanging if crun hasn't opened write end yet)
	stdoutR, err := os.OpenFile(stdoutPath, os.O_RDONLY|syscallO_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	stderrR, err := os.OpenFile(stderrPath, os.O_RDONLY|syscallO_NONBLOCK, 0)
	if err != nil {
		stdoutR.Close()
		return nil, nil, err
	}
	return stdoutR, stderrR, nil
}

// mkfifo wraps the syscall.
func mkfifo(path string, mode uint32) error {
	return exec.Command("mkfifo", path).Run()
}

const syscallO_NONBLOCK = 0o4000 // O_NONBLOCK on Linux
