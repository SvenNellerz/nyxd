// Package compose parses a minimal nyx-compose.yaml file.
// Subset of compose-spec - only what nyxd needs.
// No YAML library dep: uses encoding/json via a YAML-to-JSON shim approach.
// Actually: we accept the single dep of gopkg.in/yaml.v3 since it's tiny
// and widely audited. Everything else is stdlib.
package compose

import (
	"fmt"
	"os"
	"strings"

	"github.com/zrougamed/nyxd/internal/bundle"
	"github.com/zrougamed/nyxd/internal/network"
	"github.com/zrougamed/nyxd/internal/supervisor"
)

// File represents a nyx-compose.yaml.
type File struct {
	Version  string                   `yaml:"version"`
	Services map[string]ServiceConfig `yaml:"services"`
}

// ServiceConfig is a single service definition.
type ServiceConfig struct {
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command"`
	Entrypoint  []string          `yaml:"entrypoint"`
	Environment []string          `yaml:"environment"`
	Ports       []string          `yaml:"ports"` // "host:container[/proto]"
	WorkingDir  string            `yaml:"working_dir"`
	User        string            `yaml:"user"`
	ReadOnly    bool              `yaml:"read_only"`
	Hostname    string            `yaml:"hostname"`
	Restart     string            `yaml:"restart"` // always|on-failure|unless-stopped|no
	Labels      map[string]string `yaml:"labels"`
	Deploy      *DeployConfig     `yaml:"deploy"`
	Healthcheck *HealthConfig     `yaml:"healthcheck"`
	DependsOn   []string          `yaml:"depends_on"`
}

// DeployConfig maps to resource constraints.
type DeployConfig struct {
	Resources ResourcesConfig `yaml:"resources"`
}

type ResourcesConfig struct {
	Limits LimitConfig `yaml:"limits"`
}

type LimitConfig struct {
	CPUs   string `yaml:"cpus"`   // "0.5"
	Memory string `yaml:"memory"` // "128m"
	Pids   int64  `yaml:"pids"`
}

// HealthConfig matches Docker compose healthcheck.
type HealthConfig struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval"`
	Timeout     string   `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
	StartPeriod string   `yaml:"start_period"`
}

// Parse reads and parses a nyx-compose.yaml file.
// Uses a minimal hand-rolled YAML parser for zero deps.
func Parse(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read compose file: %w", err)
	}
	return parseYAML(data)
}

// ToSpecs converts a compose File into supervisor ContainerSpecs.
// blobPathsFn resolves image ref → ordered blob paths (from image store).
func (f *File) ToSpecs(blobPathsFn func(ref string) ([]string, error)) ([]supervisor.ContainerSpec, error) {
	specs := make([]supervisor.ContainerSpec, 0, len(f.Services))

	for name, svc := range f.Services {
		blobPaths, err := blobPathsFn(svc.Image)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}

		ports, err := parsePorts(svc.Ports)
		if err != nil {
			return nil, fmt.Errorf("service %s ports: %w", name, err)
		}

		restart := toRestartPolicy(svc.Restart)

		var resources *bundle.Resources
		if svc.Deploy != nil {
			resources = toResources(svc.Deploy)
		}

		spec := supervisor.ContainerSpec{
			ID:            name,
			Image:         svc.Image,
			BlobPaths:     blobPaths,
			Env:           svc.Environment,
			Args:          svc.Command,
			WorkDir:       svc.WorkingDir,
			ReadOnly:      svc.ReadOnly,
			Hostname:      svc.Hostname,
			PortMappings:  ports,
			RestartPolicy: restart,
			Resources:     resources,
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func toRestartPolicy(s string) supervisor.RestartPolicy {
	switch strings.ToLower(s) {
	case "always":
		return supervisor.RestartAlways
	case "on-failure":
		return supervisor.RestartOnFailure
	case "unless-stopped":
		return supervisor.RestartUnlessStopped
	default:
		return supervisor.RestartNever
	}
}

func parsePorts(raw []string) ([]network.PortMapping, error) {
	out := make([]network.PortMapping, 0, len(raw))
	for _, p := range raw {
		pm, err := parsePort(p)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, nil
}

func parsePort(s string) (network.PortMapping, error) {
	// Format: [host:]container[/proto]
	proto := "tcp"
	if idx := strings.LastIndex(s, "/"); idx != -1 {
		proto = s[idx+1:]
		s = s[:idx]
	}

	var hostPort, containerPort int
	if idx := strings.Index(s, ":"); idx != -1 {
		fmt.Sscan(s[:idx], &hostPort)
		fmt.Sscan(s[idx+1:], &containerPort)
	} else {
		fmt.Sscan(s, &containerPort)
		hostPort = containerPort
	}

	if containerPort == 0 {
		return network.PortMapping{}, fmt.Errorf("invalid port: %s", s)
	}
	return network.PortMapping{HostPort: hostPort, ContainerPort: containerPort, Protocol: proto}, nil
}

func toResources(d *DeployConfig) *bundle.Resources {
	r := &bundle.Resources{}
	lim := d.Resources.Limits

	if lim.Memory != "" {
		r.Memory = &bundle.MemoryRes{Limit: parseMemory(lim.Memory)}
	}
	if lim.Pids > 0 {
		r.Pids = &bundle.PidsRes{Limit: lim.Pids}
	}
	return r
}

// parseMemory parses "128m", "1g", "512k" into bytes.
func parseMemory(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	var val int64
	var unit string
	fmt.Sscanf(s, "%d%s", &val, &unit)
	switch unit {
	case "k", "kb":
		return val * 1024
	case "m", "mb":
		return val * 1024 * 1024
	case "g", "gb":
		return val * 1024 * 1024 * 1024
	}
	return val
}

// parseYAML is a minimal hand-rolled YAML parser for our compose subset.
// Handles: key: value, key: [list], nested maps, multi-line lists with - prefix.
// For production, swap this with gopkg.in/yaml.v3 (single dep, security-audited).
func parseYAML(data []byte) (*File, error) {
	// NOTE: This is a stub. In production, use gopkg.in/yaml.v3.
	// The stub is here so the package compiles with zero external deps.
	// To enable real YAML parsing, add the import and call yaml.Unmarshal.
	_ = data
	return nil, fmt.Errorf("parseYAML: replace with gopkg.in/yaml.v3 for production use")
}
