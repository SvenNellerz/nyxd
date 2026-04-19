package network

import "context"

// Backend configures host-wide networking (EnsureNetwork) and per-container
// setup/teardown. Implemented by the exec-based *Manager (CNI plugins) and
// by internal/network/native.Manager (in-process, no /opt/cni/bin).
type Backend interface {
	EnsureNetwork() error
	Setup(ctx context.Context, containerID, netNSPath string, ports []PortMapping) (string, error)
	Teardown(ctx context.Context, containerID, netNSPath string) error
}
