# nyxd

Minimal OCI container orchestrator for NyxOS edge nodes.

**Zero Docker. Zero Podman. Zero containerd.**

Uses `crun` as the OCI runtime and CNI plugins for networking.

## Architecture

```
nyxd
├── image/        OCI image puller (raw HTTP, no registry SDK)
├── overlay/      overlayfs rootfs management (syscall direct)
├── network/      CNI plugin executor (exec, no library)
├── bundle/       OCI runtime-spec config.json generator
├── runtime/      crun CLI wrapper
├── supervisor/   restart policy + lifecycle management
├── health/       exec/http/tcp healthchecks
├── log/          container log streaming
└── compose/      nyx-compose.yaml parser
```

## Dependencies (external)

| Dep | Why | CVE surface |
|-----|-----|-------------|
| `crun` | OCI runtime | Contained, rootless-capable |
| CNI plugins | Network setup (bridge, portmap, firewall) | Static binaries |

Go external modules: **2**
- `github.com/opencontainers/image-spec` - OCI type definitions only
- `github.com/opencontainers/runtime-spec` - OCI runtime-spec types
- `golang.org/x/sys` - Linux syscall wrappers

Everything else: **stdlib only**.

## Build

```bash
# Standard
make build

# Static binary for edge nodes (no libc)
make build-static

# Cross-compile for arm64 (Raspberry Pi, Jetson)
make build-arm64
```

## Install

```bash
# Install crun
make install-crun

# Install CNI plugins (bridge, portmap, firewall, tuning)
make install-cni

# Install daemon
make install

# Enable systemd service
cp nyxd.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now nyxd
```

## Usage

```bash
# Pull an image
nyxd image pull nginx:alpine

# Run a container
nyxd run --name web nginx:alpine

# Use a compose file
nyxd compose up -f /etc/nyxd/nyx-compose.yaml

# List containers
nyxd ps

# Logs
nyxd logs web --tail 50
```

## Security posture

- `NoNewPrivileges=true` in every container spec
- Minimal capability set (no CAP_SYS_ADMIN, no CAP_SYS_PTRACE)
- `/proc`, `/sys` masked and read-only paths enforced
- Network namespace per container (CNI bridge, ipmasq)
- overlayfs read-only lower layers
- Digest verification on every pulled blob (sha256)
- Atomic writes everywhere (write-to-tmp, rename)
- No goroutine leaks: all goroutines tied to context
- No memory leaks: explicit cleanup on container removal

## Data layout

```
/var/lib/nyxd/
├── images/
│   ├── blobs/sha256/<hex>        # compressed layer blobs
│   └── images/<repo>/<tag>/     # manifest.json + config.json
├── overlay/
│   └── <containerID>/
│       ├── layers/0000/ ... 000N/  # extracted read-only layers
│       ├── diff/                   # writable upper layer
│       ├── work/                   # overlayfs workdir
│       └── merged/                 # container rootfs (mount point)
├── bundles/<containerID>/
│   └── config.json               # OCI runtime-spec
├── run/crun/                     # crun state files
└── logs/<containerID>.log        # JSONL container logs

/run/nyxd/netns/<containerID>  # network namespace bind-mounts
```
