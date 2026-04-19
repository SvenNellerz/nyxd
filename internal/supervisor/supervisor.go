// Package supervisor manages container lifecycle: start, monitor, restart, stop.
// Implements restart policies: always, on-failure, unless-stopped, never.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/zrougamed/nyxd/internal/bundle"
	"github.com/zrougamed/nyxd/internal/health"
	"github.com/zrougamed/nyxd/internal/network"
	"github.com/zrougamed/nyxd/internal/overlay"
	"github.com/zrougamed/nyxd/internal/runtime"
	"github.com/zrougamed/nyxd/pkg/oci"
)

// RestartPolicy controls when a container is restarted after exit.
type RestartPolicy string

const (
	RestartAlways        RestartPolicy = "always"
	RestartOnFailure     RestartPolicy = "on-failure"
	RestartUnlessStopped RestartPolicy = "unless-stopped"
	RestartNever         RestartPolicy = "never"
)

// ContainerSpec fully describes a container to run.
type ContainerSpec struct {
	ID             string
	Image          string
	ImageConfig    *oci.ImageConfig
	ManifestLayers []oci.Descriptor // ordered lower→upper
	BlobPaths      []string

	Env          []string
	Args         []string
	WorkDir      string
	User         *bundle.User
	Resources    *bundle.Resources
	PortMappings []network.PortMapping
	ReadOnly     bool
	Hostname     string

	RestartPolicy RestartPolicy
	MaxRestarts   int // 0 = unlimited (for always/on-failure)
	StopTimeout   time.Duration

	Healthcheck *health.Config
}

// containerEntry tracks runtime state for a supervised container.
type containerEntry struct {
	spec      ContainerSpec
	netNS     string
	ip        string
	bundleDir string
	rootFS    string
	restarts  int
	stopped   bool // intentionally stopped - don't restart
	mu        sync.Mutex
}

// Supervisor manages the full lifecycle of containers.
type Supervisor struct {
	rt      *runtime.Runtime
	ovl     *overlay.Manager
	net     *network.Manager
	log     *slog.Logger
	baseDir string // /var/lib/nyxd

	mu         sync.RWMutex
	containers map[string]*containerEntry
	wg         sync.WaitGroup
}

// New creates a Supervisor.
func New(rt *runtime.Runtime, ovl *overlay.Manager, net *network.Manager, baseDir string, log *slog.Logger) *Supervisor {
	return &Supervisor{
		rt:         rt,
		ovl:        ovl,
		net:        net,
		log:        log,
		baseDir:    baseDir,
		containers: make(map[string]*containerEntry),
	}
}

// Start launches a container according to its spec and supervises it.
func (s *Supervisor) Start(ctx context.Context, spec ContainerSpec) error {
	s.mu.Lock()
	if _, exists := s.containers[spec.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("container %s already running", spec.ID)
	}

	entry := &containerEntry{spec: spec}
	s.containers[spec.ID] = entry
	s.mu.Unlock()

	if err := s.startOnce(ctx, entry); err != nil {
		s.mu.Lock()
		delete(s.containers, spec.ID)
		s.mu.Unlock()
		return err
	}

	s.wg.Add(1)
	go s.supervise(ctx, entry)

	return nil
}

// Stop gracefully stops a container.
func (s *Supervisor) Stop(ctx context.Context, id string) error {
	s.mu.RLock()
	entry, ok := s.containers[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}

	entry.mu.Lock()
	entry.stopped = true
	entry.mu.Unlock()

	timeout := entry.spec.StopTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sig := "TERM"
	if cfg := entry.spec.ImageConfig; cfg != nil && cfg.Config.StopSignal != "" {
		sig = cfg.Config.StopSignal
	}

	if err := s.rt.Kill(stopCtx, id, sig); err != nil {
		s.log.Warn("graceful stop failed, forcing", "id", id, "err", err)
		s.rt.Kill(ctx, id, "KILL") //nolint:errcheck
	}

	return nil
}

// Remove stops and removes a container and all its resources.
func (s *Supervisor) Remove(ctx context.Context, id string) error {
	if err := s.Stop(ctx, id); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("stop before remove", "id", id, "err", err)
	}

	s.rt.Delete(ctx, id, true) //nolint:errcheck

	s.mu.Lock()
	entry, ok := s.containers[id]
	delete(s.containers, id)
	s.mu.Unlock()

	if ok {
		s.cleanup(entry)
	}
	return nil
}

// List returns IDs of all supervised containers.
func (s *Supervisor) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.containers))
	for id := range s.containers {
		ids = append(ids, id)
	}
	return ids
}

// Shutdown stops all containers gracefully.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.containers))
	for id := range s.containers {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			stopCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			s.Stop(stopCtx, id) //nolint:errcheck
		}(id)
	}
	wg.Wait()
	s.wg.Wait()
}

// ─── Internal ──────────────────────────────────────────────────────────────────

// startOnce performs a single container start: overlay → network → bundle → crun.
func (s *Supervisor) startOnce(ctx context.Context, e *containerEntry) error {
	spec := e.spec
	log := s.log.With("id", spec.ID)

	// 1. Overlay: mount rootfs.
	rootFS, err := s.ovl.Prepare(spec.ID, spec.BlobPaths, spec.ImageConfig)
	if err != nil {
		return fmt.Errorf("overlay prepare: %w", err)
	}
	e.rootFS = rootFS

	// 2. Network namespace + CNI.
	nsPath, err := network.CreateNetNS(spec.ID)
	if err != nil {
		s.ovl.Remove(spec.ID) //nolint:errcheck
		return fmt.Errorf("netns: %w", err)
	}
	e.netNS = nsPath

	ip, err := s.net.Setup(ctx, spec.ID, nsPath, spec.PortMappings)
	if err != nil {
		network.DeleteNetNS(spec.ID) //nolint:errcheck
		s.ovl.Remove(spec.ID)        //nolint:errcheck
		return fmt.Errorf("cni setup: %w", err)
	}
	e.ip = ip
	log.Info("network assigned", "ip", ip)

	// 3. Generate OCI bundle.
	bundleDir := fmt.Sprintf("%s/bundles/%s", s.baseDir, spec.ID)
	_, err = bundle.Generate(bundleDir, bundle.Options{
		ContainerID: spec.ID,
		RootFS:      rootFS,
		NetNS:       nsPath,
		ImageConfig: spec.ImageConfig,
		Env:         spec.Env,
		Args:        spec.Args,
		WorkDir:     spec.WorkDir,
		User:        spec.User,
		Resources:   spec.Resources,
		ReadOnly:    spec.ReadOnly,
		Hostname:    spec.Hostname,
	})
	if err != nil {
		s.teardownNetwork(ctx, spec.ID)
		s.ovl.Remove(spec.ID) //nolint:errcheck
		return fmt.Errorf("bundle: %w", err)
	}
	e.bundleDir = bundleDir

	// 4. crun run --detach.
	if err := s.rt.Run(ctx, spec.ID, bundleDir); err != nil {
		s.teardownNetwork(ctx, spec.ID)
		s.ovl.Remove(spec.ID) //nolint:errcheck
		return fmt.Errorf("crun run: %w", err)
	}

	log.Info("container started", "image", spec.Image, "ip", ip)
	return nil
}

// supervise watches a container and applies restart policy.
func (s *Supervisor) supervise(ctx context.Context, e *containerEntry) {
	defer s.wg.Done()

	log := s.log.With("id", e.spec.ID)

	for {
		// Wait for container exit.
		exitCode, err := s.rt.WaitForExit(ctx, e.spec.ID)
		if err != nil {
			if ctx.Err() != nil {
				return // daemon shutting down
			}
			log.Warn("wait error", "err", err)
		}

		e.mu.Lock()
		intentionallyStopped := e.stopped
		e.mu.Unlock()

		log.Info("container exited", "exitCode", exitCode, "intentional", intentionallyStopped)

		// Cleanup network + overlay.
		s.teardownNetwork(ctx, e.spec.ID)
		s.ovl.Remove(e.spec.ID) //nolint:errcheck

		if intentionallyStopped {
			s.rt.Delete(ctx, e.spec.ID, false) //nolint:errcheck
			s.mu.Lock()
			delete(s.containers, e.spec.ID)
			s.mu.Unlock()
			return
		}

		// Apply restart policy.
		if !s.shouldRestart(e, exitCode) {
			log.Info("not restarting", "policy", e.spec.RestartPolicy, "restarts", e.restarts)
			s.rt.Delete(ctx, e.spec.ID, false) //nolint:errcheck
			s.mu.Lock()
			delete(s.containers, e.spec.ID)
			s.mu.Unlock()
			return
		}

		e.restarts++
		delay := backoff(e.restarts)
		log.Info("restarting", "attempt", e.restarts, "delay", delay)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		if err := s.startOnce(ctx, e); err != nil {
			log.Error("restart failed", "err", err)
			// Let the loop continue - next iteration will try again or give up.
			time.Sleep(5 * time.Second)
		}
	}
}

func (s *Supervisor) shouldRestart(e *containerEntry, exitCode int) bool {
	spec := e.spec

	if spec.MaxRestarts > 0 && e.restarts >= spec.MaxRestarts {
		return false
	}

	switch spec.RestartPolicy {
	case RestartAlways:
		return true
	case RestartOnFailure:
		return exitCode != 0
	case RestartUnlessStopped:
		return true
	case RestartNever, "":
		return false
	default:
		return false
	}
}

func (s *Supervisor) teardownNetwork(ctx context.Context, id string) {
	nsPath := fmt.Sprintf("/run/nyxd/netns/%s", id)
	if err := s.net.Teardown(ctx, id, nsPath); err != nil {
		s.log.Warn("cni teardown", "id", id, "err", err)
	}
	if err := network.DeleteNetNS(id); err != nil {
		s.log.Warn("netns delete", "id", id, "err", err)
	}
}

func (s *Supervisor) cleanup(e *containerEntry) {
	if e.netNS != "" {
		s.net.Teardown(context.Background(), e.spec.ID, e.netNS) //nolint:errcheck
		network.DeleteNetNS(e.spec.ID)                           //nolint:errcheck
	}
	s.ovl.Remove(e.spec.ID) //nolint:errcheck
}

// backoff returns an exponential backoff delay capped at 30s, with small jitter.
func backoff(attempt int) time.Duration {
	d := time.Duration(attempt) * time.Duration(attempt) * 100 * time.Millisecond
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	jitter := time.Duration(rand.Int64N(int64(d/10 + 1))) // up to ~10% extra
	return d + jitter
}
