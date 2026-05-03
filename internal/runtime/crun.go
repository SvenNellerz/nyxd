// Package runtime wraps crun (or runc) as an OCI runtime.
// All communication is via crun's CLI - no shared library, no CGO.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// ExitCode is populated by some runtimes (e.g. crun) when status is stopped.
	ExitCode *int `json:"exit_code,omitempty"`
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

// RunForeground runs the container in the foreground: crun blocks until the init
// process exits. Container stdout/stderr are wired to the given writers (typically
// pipes feeding a log collector). No --detach flag. Uses --pid-file so OCI state
// under --root matches detached runs (some crun versions rely on this for status).
func (r *Runtime) RunForeground(ctx context.Context, containerID, bundleDir string, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	pidFile := filepath.Join(r.rootDir, containerID+".pid")
	cmd := exec.CommandContext(ctx, r.binary,
		"--root", r.rootDir,
		"run",
		"--bundle", bundleDir,
		"--pid-file", pidFile,
		containerID,
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	var errBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(stderr, &errBuf)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return fmt.Errorf("crun run: %w: %s", err, msg)
		}
		return fmt.Errorf("crun run: %w", err)
	}
	return nil
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
	_ = os.Remove(filepath.Join(r.rootDir, containerID+".pid"))
	if err != nil && isCrunAbsent(err) {
		return nil
	}
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
				// Only treat as exit if crun no longer has this id (e.g. after delete).
				// Transient state errors must not tear down a still-running container.
				if isCrunAbsent(err) {
					return -1, nil
				}
				continue
			}
			if s.Status == "stopped" {
				return r.resolveExitCode(ctx, containerID, s), nil
			}
		}
	}
}

// WaitStopped blocks until crun reports status "stopped" or the container id
// disappears from crun (fully removed). Used after kill so callers do not
// assume the process is gone while the runtime still reports "running".
func (r *Runtime) WaitStopped(ctx context.Context, containerID string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s, err := r.State(ctx, containerID)
			if err != nil {
				if isCrunAbsent(err) {
					return nil
				}
				continue
			}
			if s.Status == "stopped" {
				return nil
			}
		}
	}
}

func isCrunAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") ||
		strings.Contains(msg, "cannot find") ||
		strings.Contains(msg, "could not find") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "unable to find") {
		return true
	}
	// e.g. crun: error opening file '.../run/crun/<id>/status': No such file or directory
	if strings.Contains(msg, "no such file or directory") && strings.Contains(msg, "status") {
		return true
	}
	return false
}

// CrunContainerAbsent reports whether crun says this container id has no OCI state
// under this runtime's --root (including a missing .../status file).
func CrunContainerAbsent(err error) bool {
	return isCrunAbsent(err)
}

func (r *Runtime) resolveExitCode(ctx context.Context, containerID string, s *State) int {
	if s != nil && s.ExitCode != nil {
		return *s.ExitCode
	}
	if code, ok := exitCodeFromStateFile(filepath.Join(r.rootDir, containerID, "exit_code")); ok {
		return code
	}
	// Re-fetch raw state for extensions not mapped on State (best-effort).
	if code, ok := r.exitCodeFromRawState(ctx, containerID); ok {
		return code
	}
	return -1
}

func exitCodeFromStateFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return code, true
}

func (r *Runtime) exitCodeFromRawState(ctx context.Context, containerID string) (int, bool) {
	args := []string{"--root", r.rootDir, "state", containerID}
	var buf bytes.Buffer
	if err := r.run(ctx, args, &buf); err != nil {
		return 0, false
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		return 0, false
	}
	for _, key := range []string{"exit_code", "exitCode", "exit_status", "exitStatus"} {
		if v, ok := m[key]; ok {
			switch t := v.(type) {
			case float64:
				return int(t), true
			}
		}
	}
	return 0, false
}

// Exec runs a one-shot process inside a running container (crun exec).
// If stdin is non-nil, it is wired to crun's stdin (streaming).
func (r *Runtime) Exec(ctx context.Context, containerID string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("exec: empty argv")
	}
	args := append([]string{"--root", r.rootDir, "exec", "--", containerID}, argv...)
	cmd := exec.CommandContext(ctx, r.binary, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	switch {
	case stdout != nil && stderr != nil:
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	case stdout != nil:
		cmd.Stdout = stdout
		cmd.Stderr = stdout
	case stderr != nil:
		cmd.Stdout = io.Discard
		cmd.Stderr = stderr
	default:
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crun exec: %w", err)
	}
	return nil
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
