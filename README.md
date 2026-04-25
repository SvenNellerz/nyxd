# nyxd

Minimal OCI container orchestrator for NyxOS edge nodes.

**Zero Docker. Zero Podman. Zero containerd.**

Uses `crun` as the OCI runtime. **Container networking** defaults to in-process native mode; optional CNI plugins are supported. See **[docs/networking.md](docs/networking.md)** for `-net-driver`, `network.Backend`, the startup log line, `nyxd.service`, and examples.

## Architecture

```
nyxd
├── image/        OCI image puller (raw HTTP, no registry SDK)
├── overlay/      overlayfs rootfs management (syscall direct)
├── network/      Networking: native (default) or CNI plugin executor
├── network/native/  In-process bridge + veth + IPAM (no /opt/cni/bin)
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
| CNI plugins | Only if you pass **`-net-driver=cni`** | Static binaries under `/opt/cni/bin` |
| `nft` | Port NAT rules (native driver) | System `nft` binary |

Go external modules: **2**
- `github.com/opencontainers/image-spec` - OCI type definitions only
- `github.com/opencontainers/runtime-spec` - OCI runtime-spec types
- `golang.org/x/sys` - Linux syscall wrappers

Everything else: **stdlib only**.

## Networking (native vs CNI)

- **Default:** `nyxd --net-driver=native` (implicit if omitted). No `/opt/cni/bin` required. Startup logs include `"network backend","driver":"native"`.
- **Optional CNI:** `nyxd -net-driver=cni -cni-bin-dir=/opt/cni/bin ...` after installing plugins (e.g. `make install-cni`).
- **Client:** `nyx` does not choose the driver; restart **`nyxd`** after changing flags.
- **`nyx run`:** waits after start; **Ctrl+C** stops the container via the control API. Use **`nyx run -d`** for detach, or **`nyx stop <id>`**.

Full detail: **[docs/networking.md](docs/networking.md)**.

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

# Optional: CNI plugins only when using -net-driver=cni
# make install-cni

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
- Network namespace per container (bridge + NAT; native driver by default)
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
