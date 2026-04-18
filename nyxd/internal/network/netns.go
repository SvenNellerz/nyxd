package network

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const netNSDir = "/var/run/netns"

// CreateNetNS creates a named network namespace for containerID.
// Returns the path to the netns file (e.g. /var/run/netns/<containerID>).
// The caller must call DeleteNetNS when the container exits.
func CreateNetNS(containerID string) (string, error) {
	if err := os.MkdirAll(netNSDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir netns dir: %w", err)
	}

	nsPath := filepath.Join(netNSDir, containerID)

	// Create a bind-mount target file
	f, err := os.OpenFile(nsPath, os.O_CREATE|os.O_RDONLY, 0o444)
	if err != nil {
		return "", fmt.Errorf("create netns file: %w", err)
	}
	f.Close()

	// Lock OS thread — Linux namespace operations are per-thread
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save current netns fd
	origNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return "", fmt.Errorf("open current netns: %w", err)
	}
	defer origNS.Close()

	// Create new network namespace
	if err := syscall.Unshare(syscall.CLONE_NEWNET); err != nil {
		return "", fmt.Errorf("unshare net ns: %w", err)
	}

	// Bind-mount /proc/self/ns/net → nsPath so it persists after we exit
	if err := syscall.Mount("/proc/self/ns/net", nsPath, "", syscall.MS_BIND, ""); err != nil {
		// Try to restore the original namespace before returning error
		_ = setNetNS(origNS)
		return "", fmt.Errorf("bind mount netns: %w", err)
	}

	// Restore original network namespace
	if err := setNetNS(origNS); err != nil {
		return "", fmt.Errorf("restore netns: %w", err)
	}

	slog.Debug("network namespace created", "container", containerID, "path", nsPath)
	return nsPath, nil
}

// DeleteNetNS unmounts and removes the network namespace file.
func DeleteNetNS(containerID string) error {
	nsPath := filepath.Join(netNSDir, containerID)

	if err := syscall.Unmount(nsPath, syscall.MNT_DETACH); err != nil {
		if errno, ok := err.(syscall.Errno); ok && (errno == syscall.ENOENT || errno == syscall.EINVAL) {
			return nil // already gone
		}
		return fmt.Errorf("unmount netns %s: %w", nsPath, err)
	}

	if err := os.Remove(nsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove netns file: %w", err)
	}

	slog.Debug("network namespace deleted", "container", containerID)
	return nil
}

// setNetNS switches the current goroutine's thread to the given network namespace.
// Must be called with runtime.LockOSThread held.
func setNetNS(f *os.File) error {
	// setns(2) with CLONE_NEWNET
	const CLONE_NEWNET = 0x40000000
	_, _, errno := syscall.RawSyscall(syscall.SYS_SETNS, f.Fd(), CLONE_NEWNET, 0)
	if errno != 0 {
		return fmt.Errorf("setns: %w", errno)
	}
	return nil
}
