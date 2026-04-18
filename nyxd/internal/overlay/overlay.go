// Package overlay manages overlayfs mounts for container rootfs.
// Uses kernel overlayfs directly via syscall - no external tools needed.
package overlay

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zrougamed/nyxd/pkg/oci"
)

// Manager handles overlayfs creation and teardown for containers.
//
// Layout per container:
//
//	<base>/<containerID>/
//	  layers/  – extracted read-only layer dirs (bottom → top)
//	  diff/    – container writable layer (upperdir)
//	  work/    – overlayfs workdir (kernel requirement)
//	  merged/  – merged mount point (container rootfs)
type Manager struct {
	base string // e.g. /var/lib/nyxd/overlay
}

// NewManager creates an overlay manager rooted at base.
func NewManager(base string) (*Manager, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("overlay manager init: %w", err)
	}
	return &Manager{base: base}, nil
}

// Prepare unpacks image layers and mounts an overlayfs for containerID.
// blobPaths is ordered lower→upper (first layer is bottom of stack).
// Returns the merged directory path (container rootfs).
func (m *Manager) Prepare(containerID string, blobPaths []string, cfg *oci.ImageConfig) (string, error) {
	cDir := filepath.Join(m.base, containerID)
	layersDir := filepath.Join(cDir, "layers")
	diffDir := filepath.Join(cDir, "diff")
	workDir := filepath.Join(cDir, "work")
	mergedDir := filepath.Join(cDir, "merged")

	for _, d := range []string{layersDir, diffDir, workDir, mergedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("overlay dirs: %w", err)
		}
	}

	// Extract each layer tarball into its own directory.
	lowerDirs := make([]string, 0, len(blobPaths))
	for i, blobPath := range blobPaths {
		ldir := filepath.Join(layersDir, fmt.Sprintf("%04d", i))
		if err := os.MkdirAll(ldir, 0o755); err != nil {
			return "", err
		}
		if err := extractTar(blobPath, ldir); err != nil {
			return "", fmt.Errorf("extract layer %d: %w", i, err)
		}
		lowerDirs = append(lowerDirs, ldir)
	}

	// overlayfs lowerdir is colon-separated, upper-most layer listed first.
	reversedLowers := reverseStrings(lowerDirs)
	lowerOpt := strings.Join(reversedLowers, ":")
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerOpt, diffDir, workDir)

	if err := syscall.Mount("overlay", mergedDir, "overlay", 0, opts); err != nil {
		return "", fmt.Errorf("overlayfs mount: %w", err)
	}

	return mergedDir, nil
}

// Remove unmounts and cleans up all overlayfs state for containerID.
func (m *Manager) Remove(containerID string) error {
	cDir := filepath.Join(m.base, containerID)
	mergedDir := filepath.Join(cDir, "merged")

	// Unmount - ignore EINVAL (already unmounted).
	if err := syscall.Unmount(mergedDir, syscall.MNT_DETACH); err != nil {
		if err != syscall.EINVAL && err != syscall.ENOENT {
			return fmt.Errorf("overlay unmount %s: %w", containerID, err)
		}
	}

	return os.RemoveAll(cDir)
}

// MergedDir returns the merged mount path for a container without mounting.
func (m *Manager) MergedDir(containerID string) string {
	return filepath.Join(m.base, containerID, "merged")
}

// IsMounted returns true if the merged dir has an active overlayfs mount.
func (m *Manager) IsMounted(containerID string) bool {
	mergedDir := filepath.Join(m.base, containerID, "merged")
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), mergedDir)
}

// extractTar unpacks a (possibly gzip-compressed) tar archive into dest.
// Uses the system tar binary to handle whiteout files correctly.
func extractTar(src, dest string) error {
	// We use system tar: it handles OCI whiteouts (.wh. files) and
	// is more robust than a pure-Go implementation for edge cases.
	// --no-same-owner so we don't need CAP_CHOWN for every file.
	cmd := exec.Command("tar",
		"--no-same-owner",
		"--no-same-permissions",
		"-xf", src,
		"-C", dest,
	)
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar extract: %w", err)
	}

	// Process OCI whiteout files.
	return processWhiteouts(dest)
}

// processWhiteouts handles OCI layer whiteout semantics:
//   - .wh.<name>     → delete <name> in the same directory
//   - .wh..wh..opq  → opaque directory (nothing to do for overlayfs upperdir,
//     kernel handles it via trusted.overlay.opaque xattr)
func processWhiteouts(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, ".wh.") {
			return nil
		}
		if base == ".wh..wh..opq" {
			// Opaque whiteout - remove the marker, overlayfs handles it via xattr.
			return os.Remove(path)
		}
		// Regular whiteout: remove the marker and the target file.
		target := filepath.Join(filepath.Dir(path), strings.TrimPrefix(base, ".wh."))
		os.RemoveAll(target) // best-effort
		return os.Remove(path)
	})
}

func reverseStrings(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}
