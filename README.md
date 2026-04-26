# nyxd

[![CI](https://github.com/zrougamed/nyxd/actions/workflows/ci.yml/badge.svg)](https://github.com/zrougamed/nyxd/actions/workflows/ci.yml)
[![Release](https://github.com/zrougamed/nyxd/actions/workflows/release.yml/badge.svg)](https://github.com/zrougamed/nyxd/actions/workflows/release.yml)

Minimal OCI container orchestrator for **Linux** (x86_64, arm64, and similar) — **`crun`** plus a small control plane, no Docker/Podman/containerd. It started as a lightweight way to run containers on a home **Raspberry Pi** without a heavy stack; the same design runs on **any** capable Linux host (VPS, laptop, edge box, lab VM).

**Zero Docker. Zero Podman. Zero containerd.**

Uses `crun` as the OCI runtime. **Networking** defaults to in-process **native** mode; optional **CNI** plugins are supported. See **[docs/networking.md](docs/networking.md)** for `-net-driver`, the startup log line, and `nyx run` / stop behavior.

**Docs index:** **[docs/README.md](docs/README.md)** — install, usage, OpenAPI, QEMU Alpine, kernel notes.

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

Go modules (see `go.mod`): OCI spec packages, `gopkg.in/yaml.v3`, `golang.org/x/sys`, **`github.com/hedzr/progressbar`** (nyx pull UI), plus transitive deps.

## Networking (native vs CNI)

- **Default:** `nyxd --net-driver=native` (implicit if omitted). No `/opt/cni/bin` required. Startup logs include `"network backend","driver":"native"`.
- **Optional CNI:** `nyxd -net-driver=cni -cni-bin-dir=/opt/cni/bin ...` after installing plugins (e.g. `make install-cni`).
- **Client:** `nyx` does not choose the driver; restart **`nyxd`** after changing flags.
- **`nyx run`:** waits after start; **Ctrl+C** stops the container via the control API. Use **`nyx run -d`** for detach, or **`nyx stop <id>`**.

Full detail: **[docs/networking.md](docs/networking.md)**.

## Build

```bash
# Standard (Linux or cross-compiling from your dev machine)
make build
make build-nyx

# Static Linux amd64 binary (useful for minimal rootfs)
make build-static

# Cross-compile daemon for Linux arm64 (e.g. Raspberry Pi 64-bit, Graviton)
make build-arm64
```

## Releases (GitHub Actions)

Create a tag whose name starts with `v` (for example `v0.2.0`) and push it to GitHub. The **Release** workflow runs the same checks as **CI**, then publishes a GitHub Release with `nyxd` and `nyx` binaries for **linux/amd64** and **linux/arm64**, plus `sha256` sidecars.

## Install and usage

See **[docs/INSTALL.md](docs/INSTALL.md)** (Linux install, systemd, cross-build) and **[docs/USAGE.md](docs/USAGE.md)** (`nyx` + `curl`).

API contract: **[docs/openapi.yaml](docs/openapi.yaml)**.

## Security scans (repo checkout)

```bash
make scan          # Trivy + Grype (tools must be on PATH)
make scan-trivy
make scan-grype
```

## Try in QEMU first

**[docs/qemu-alpine.md](docs/qemu-alpine.md)** and **`scripts/qemu-alpine-nyxd.sh`** — boot **Alpine 3.23** virt media under QEMU to try nyxd safely before bare metal (e.g. an SBC SD card).

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
