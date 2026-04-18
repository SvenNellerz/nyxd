// Package runtime builds OCI runtime specs and invokes crun.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/zrougamed/nyxd/internal/compose"
	"github.com/zrougamed/nyxd/internal/image"
)

// BuildSpec constructs an OCI runtime spec for a container.
// rootfs is the merged overlayfs directory.
// netNSPath is the network namespace to join.
func BuildSpec(
	containerID string,
	svc compose.Service,
	imgCfg *image.ImageConfig,
	rootfs string,
	netNSPath string,
	ips []string,
) (*ocispec.Spec, error) {
	spec := baseSpec()

	// Rootfs
	spec.Root = &ocispec.Root{
		Path:     rootfs,
		Readonly: svc.ReadOnly,
	}

	// Process
	proc, err := buildProcess(svc, imgCfg)
	if err != nil {
		return nil, err
	}
	spec.Process = proc

	// Hostname = container name (service name)
	spec.Hostname = containerID

	// Mounts
	spec.Mounts = buildMounts(svc)

	// Linux namespaces
	spec.Linux = buildLinux(svc, netNSPath)

	// Resource limits
	if svc.Deploy != nil {
		applyResources(spec, svc.Deploy)
	}

	// Seccomp
	if svc.SeccompProfile != "" {
		if err := applySeccomp(spec, svc.SeccompProfile); err != nil {
			return nil, fmt.Errorf("apply seccomp: %w", err)
		}
	}

	return spec, nil
}

// WriteSpec serialises spec to <bundleDir>/config.json.
func WriteSpec(bundleDir string, spec *ocispec.Spec) error {
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), data, 0o600)
}

// -------------------------------------------------------------------
// Process
// -------------------------------------------------------------------

func buildProcess(svc compose.Service, imgCfg *image.ImageConfig) (*ocispec.Process, error) {
	env := mergeEnv(imgCfg.Config.Env, svc.Environment)

	entrypoint := imgCfg.Config.Entrypoint
	if len(svc.Entrypoint) > 0 {
		entrypoint = svc.Entrypoint
	}
	args := imgCfg.Config.Cmd
	if len(svc.Command) > 0 {
		args = svc.Command
	}
	argv := append(entrypoint, args...)
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command or entrypoint defined")
	}

	cwd := imgCfg.Config.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	noNewPrivs := true
	if svc.NoNewPrivileges != nil {
		noNewPrivs = *svc.NoNewPrivileges
	}

	proc := &ocispec.Process{
		Terminal:        false,
		Args:            argv,
		Env:             env,
		Cwd:             cwd,
		NoNewPrivileges: noNewPrivs,
		Capabilities:    buildCapabilities(svc),
		Rlimits: []ocispec.POSIXRlimit{
			{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024},
		},
	}

	if svc.User != "" {
		uid, gid, err := parseUser(svc.User)
		if err != nil {
			return nil, fmt.Errorf("parse user: %w", err)
		}
		proc.User = ocispec.User{UID: uid, GID: gid}
	}

	return proc, nil
}

func parseUser(user string) (uint32, uint32, error) {
	parts := strings.SplitN(user, ":", 2)
	uid, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("uid %q: %w", parts[0], err)
	}
	var gid uint64
	if len(parts) == 2 {
		gid, err = strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("gid %q: %w", parts[1], err)
		}
	}
	return uint32(uid), uint32(gid), nil
}

func mergeEnv(imageEnv []string, svcEnv map[string]string) []string {
	// Start with image env, then overlay service env
	kvMap := make(map[string]string, len(imageEnv)+len(svcEnv))
	for _, kv := range imageEnv {
		k, v, _ := strings.Cut(kv, "=")
		kvMap[k] = v
	}
	for k, v := range svcEnv {
		kvMap[k] = v
	}
	result := make([]string, 0, len(kvMap))
	for k, v := range kvMap {
		result = append(result, k+"="+v)
	}
	return result
}

// -------------------------------------------------------------------
// Capabilities
// -------------------------------------------------------------------

// defaultDropCaps are dropped from all containers unless explicitly added back.
var defaultDropCaps = []string{
	"CAP_NET_RAW",
	"CAP_SYS_MODULE",
	"CAP_SYS_RAWIO",
	"CAP_SYS_PTRACE",
	"CAP_SYS_ADMIN",
	"CAP_MKNOD",
	"CAP_AUDIT_WRITE",
	"CAP_AUDIT_CONTROL",
	"CAP_MAC_OVERRIDE",
	"CAP_MAC_ADMIN",
	"CAP_SYSLOG",
	"CAP_NET_ADMIN",
	"CAP_SYS_TIME",
	"CAP_SYS_TTY_CONFIG",
	"CAP_DAC_READ_SEARCH",
	"CAP_IPC_LOCK",
	"CAP_LINUX_IMMUTABLE",
}

// defaultKeepCaps are the capabilities non-privileged containers retain.
var defaultKeepCaps = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_NET_BIND_SERVICE",
	"CAP_KILL",
	"CAP_SETPCAP",
}

func buildCapabilities(svc compose.Service) *ocispec.LinuxCapabilities {
	if svc.Privileged {
		// Privileged: all caps (avoid unless absolutely necessary)
		all := allCapabilities()
		return &ocispec.LinuxCapabilities{
			Bounding:    all,
			Effective:   all,
			Permitted:   all,
			Inheritable: []string{},
			Ambient:     []string{},
		}
	}

	caps := make(map[string]struct{}, len(defaultKeepCaps))
	for _, c := range defaultKeepCaps {
		caps[c] = struct{}{}
	}
	// Apply cap_add
	for _, c := range svc.CapAdd {
		caps[normaliseCAP(c)] = struct{}{}
	}
	// Apply cap_drop
	for _, c := range svc.CapDrop {
		delete(caps, normaliseCAP(c))
	}

	final := make([]string, 0, len(caps))
	for c := range caps {
		final = append(final, c)
	}

	return &ocispec.LinuxCapabilities{
		Bounding:    final,
		Effective:   final,
		Permitted:   final,
		Inheritable: []string{},
		Ambient:     []string{},
	}
}

func normaliseCAP(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "CAP_") {
		return "CAP_" + s
	}
	return s
}

func allCapabilities() []string {
	// Linux 5.x full set (last capability = CAP_CHECKPOINT_RESTORE = 40)
	caps := make([]string, 0, 41)
	for i := 0; i <= 40; i++ {
		caps = append(caps, fmt.Sprintf("CAP_%d", i))
	}
	return caps
}

// -------------------------------------------------------------------
// Mounts
// -------------------------------------------------------------------

func buildMounts(svc compose.Service) []ocispec.Mount {
	mounts := []ocispec.Mount{
		// Essential procfs
		{Destination: "/proc", Type: "proc", Source: "proc"},
		// tmpfs for /dev
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		// devpts
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts",
			Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		// shm
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm",
			Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		// /sys (read-only)
		{Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		// /sys/fs/cgroup
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup",
			Options: []string{"nosuid", "noexec", "nodev", "relatime"}},
		// /etc/resolv.conf
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/etc/resolv.conf",
			Options: []string{"rbind", "ro"}},
		// /etc/hosts
		{Destination: "/etc/hosts", Type: "bind", Source: "/etc/hosts",
			Options: []string{"rbind", "ro"}},
	}

	for _, v := range svc.Volumes {
		m := parseVolumeMount(v)
		if m != nil {
			mounts = append(mounts, *m)
		}
	}

	return mounts
}

func parseVolumeMount(v string) *ocispec.Mount {
	parts := strings.SplitN(v, ":", 3)
	if len(parts) < 2 {
		return nil
	}
	opts := []string{"rbind"}
	if len(parts) == 3 && parts[2] == "ro" {
		opts = append(opts, "ro")
	} else {
		opts = append(opts, "rw")
	}
	return &ocispec.Mount{
		Destination: parts[1],
		Source:      parts[0],
		Type:        "bind",
		Options:     opts,
	}
}

// -------------------------------------------------------------------
// Linux namespace + cgroup configuration
// -------------------------------------------------------------------

func buildLinux(svc compose.Service, netNSPath string) *ocispec.Linux {
	namespaces := []ocispec.LinuxNamespace{
		{Type: ocispec.MountNamespace},
		{Type: ocispec.PIDNamespace},
		{Type: ocispec.IPCNamespace},
		{Type: ocispec.UTSNamespace},
		{Type: ocispec.CgroupNamespace},
	}

	if netNSPath != "" {
		namespaces = append(namespaces, ocispec.LinuxNamespace{
			Type: ocispec.NetworkNamespace,
			Path: netNSPath,
		})
	} else {
		namespaces = append(namespaces, ocispec.LinuxNamespace{Type: ocispec.NetworkNamespace})
	}

	if !svc.Privileged {
		namespaces = append(namespaces, ocispec.LinuxNamespace{Type: ocispec.UserNamespace})
	}

	linux := &ocispec.Linux{
		Namespaces: namespaces,
		// Masked paths — hide sensitive host paths
		MaskedPaths: []string{
			"/proc/acpi",
			"/proc/asound",
			"/proc/kcore",
			"/proc/keys",
			"/proc/latency_stats",
			"/proc/timer_list",
			"/proc/timer_stats",
			"/proc/sched_debug",
			"/sys/firmware",
			"/proc/scsi",
		},
		// Read-only paths
		ReadonlyPaths: []string{
			"/proc/bus",
			"/proc/fs",
			"/proc/irq",
			"/proc/sys",
			"/proc/sysrq-trigger",
		},
	}

	return linux
}

func applyResources(spec *ocispec.Spec, deploy *compose.Deploy) {
	if spec.Linux == nil {
		return
	}
	res := &ocispec.LinuxResources{}

	// Memory limit
	if deploy.Resources.Limits.Memory != "" {
		mem := parseMemory(deploy.Resources.Limits.Memory)
		if mem > 0 {
			res.Memory = &ocispec.LinuxMemory{Limit: &mem}
		}
	}

	// CPU quota — convert "0.5" CPUs to CPU period/quota
	if deploy.Resources.Limits.CPUs != "" {
		quota, period := parseCPU(deploy.Resources.Limits.CPUs)
		if quota > 0 {
			res.CPU = &ocispec.LinuxCPU{
				Quota:  &quota,
				Period: &period,
			}
		}
	}

	spec.Linux.Resources = res
}

func parseMemory(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	mul := int64(1)
	if strings.HasSuffix(s, "g") {
		mul = 1 << 30
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "m") {
		mul = 1 << 20
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "k") {
		mul = 1 << 10
		s = s[:len(s)-1]
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v * mul
}

func parseCPU(s string) (quota, period int64) {
	period = 100000 // 100ms
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	quota = int64(f * float64(period))
	return quota, period
}

func applySeccomp(spec *ocispec.Spec, profilePath string) error {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("read seccomp profile: %w", err)
	}
	var profile ocispec.LinuxSeccomp
	if err := json.Unmarshal(data, &profile); err != nil {
		return fmt.Errorf("parse seccomp profile: %w", err)
	}
	if spec.Linux == nil {
		spec.Linux = &ocispec.Linux{}
	}
	spec.Linux.Seccomp = &profile
	return nil
}

// -------------------------------------------------------------------
// Base spec
// -------------------------------------------------------------------

func baseSpec() *ocispec.Spec {
	return &ocispec.Spec{
		Version: "1.2.0",
	}
}
