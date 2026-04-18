// Package runtime wraps crun (or runc) as an OCI runtime.
// All communication is via crun's CLI - no shared library, no CGO.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// State mirrors crun's container state output.
type State struct {
	ID          string `json:"id"`
	Status      string `json:"status"` // created, running, stopped
	Pid         int    `json:"pid"`
	Bundle      string `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Runtime wraps crun/runc CLI.
type Runtime struct {
	binary  string // path to crun or runc
	rootDir string // --root: crun state directory
}

// New creates a Runtime using the given binary (e.g. "/usr/bin/crun").
// stateDir is where crun keeps its state (/run/nyxd/crun).
func New(binary, stateDir string) (*Runtime, error) {
	if _, err := os.Stat(binary); err != nil {
		// Try PATH lookup.
		path, err := exec.LookPath(binary)
		if err != nil {
			return nil, fmt.Errorf("runtime binary not found: %s", binary)
		}
		binary = path
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime state dir: %w", err)
	}
	return &Runtime{binary: binary, rootDir: stateDir}, nil
}

// Create creates a container from a bundle without starting its process.
// Equivalent to: crun create --bundle <dir> <id>
func (r *Runtime) Create(ctx context.Context, containerID, bundleDir string) error {
	pidFile := filepath.Join(r.rootDir, containerID+".pid")
	args := []string{
		"--root", r.rootDir,
		"create",
		"--bundle", bundleDir,
		"--pid-file", pidFile,
		containerID,
	}
	return r.run(ctx, args, nil)
}

// Start runs the user-defined command in a created container.
// Equivalent to: crun start <id>
func (r *Runtime) Start(ctx context.Context, containerID string) error {
	return r.run(ctx, []string{"--root", r.rootDir, "start", containerID}, nil)
}

// Run creates and starts a container in one step (detached).
// Returns when the container's init process has started.
func (r *Runtime) Run(ctx context.Context, containerID, bundleDir string) error {
	pidFile := filepath.Join(r.rootDir, containerID+".pid")
	args := []string{
		"--root", r.rootDir,
		"run",
		"--detach",
		"--bundle", bundleDir,
		"--pid-file", pidFile,
		containerID,
	}
	return r.run(ctx, args, nil)
}

// Kill sends a signal to a container's init process.
func (r *Runtime) Kill(ctx context.Context, containerID, signal string) error {
	if signal == "" {
		signal = "TERM"
	}
	return r.run(ctx, []string{"--root", r.rootDir, "kill", containerID, signal}, nil)
}

// Delete removes a container and cleans up its state.
// force=true sends SIGKILL first.
func (r *Runtime) Delete(ctx context.Context, containerID string, force bool) error {
	args := []string{"--root", r.rootDir, "delete"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, containerID)
	err := r.run(ctx, args, nil)
	// Clean up pid file.
	os.Remove(filepath.Join(r.rootDir, containerID+".pid"))
	return err
}

// State returns current state of a container.
func (r *Runtime) State(ctx context.Context, containerID string) (*State, error) {
	args := []string{"--root", r.rootDir, "state", containerID}
	var buf bytes.Buffer
	if err := r.run(ctx, args, &buf); err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &s, nil
}

// List returns all container IDs managed by this runtime.
func (r *Runtime) List(ctx context.Context) ([]State, error) {
	args := []string{"--root", r.rootDir, "list", "--format", "json"}
	var buf bytes.Buffer
	if err := r.run(ctx, args, &buf); err != nil {
		return nil, err
	}
	var states []State
	if err := json.Unmarshal(buf.Bytes(), &states); err != nil {
		return nil, fmt.Errorf("parse list: %w", err)
	}
	return states, nil
}

// WaitForExit blocks until the container exits or ctx is cancelled.
// Returns the exit code.
func (r *Runtime) WaitForExit(ctx context.Context, containerID string) (int, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-ticker.C:
			s, err := r.State(ctx, containerID)
			if err != nil {
				// Container deleted = stopped.
				return -1, nil
			}
			if s.Status == "stopped" {
				return r.exitCode(containerID), nil
			}
		}
	}
}

// exitCode reads the exit code from crun's state dir.
func (r *Runtime) exitCode(containerID string) int {
	data, err := os.ReadFile(filepath.Join(r.rootDir, containerID, "exit_code"))
	if err != nil {
		return -1
	}
	var code int
	fmt.Sscan(strings.TrimSpace(string(data)), &code)
	return code
}

// run executes a crun command, writing stdout to out (if non-nil).
func (r *Runtime) run(ctx context.Context, args []string, out *bytes.Buffer) error {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if out != nil {
		cmd.Stdout = out
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crun %s: %w: %s", args[1], err, stderr.String())
	}
	return nil
}
