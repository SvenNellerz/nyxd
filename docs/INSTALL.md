# Installing nyxd for a Raspberry Pi lab

This guide targets a **small home lab** (one or more Raspberry Pis, or similar ARM64 Linux boards) where you run **nyxd** as the container daemon and **nyx** on the same host (or from your laptop against the socket).

## What you need on each Linux host

| Requirement | Notes |
|-------------|--------|
| **Linux** with **overlayfs**, **namespaces**, **cgroups** | Raspberry Pi OS 64-bit, Debian, Fedora ARM, etc. |
| **Root** for `nyxd` | Overlay, netns, and crun state require root today. |
| **crun** | `apt install crun`, `dnf install crun`, or build from source. |
| **Kernel modules** | See [kernel-requirements.md](kernel-requirements.md) — `overlay`, `bridge`, `veth`, `br_netfilter`, nftables NAT, etc. |
| **nft** (optional but recommended) | Used for port maps with the **native** network driver. |
| **Go 1.22+** (build only) | On the Pi or cross-compile from your PC. |

Networking defaults to **`-net-driver=native`** (no `/opt/cni/bin`). See [networking.md](networking.md).

## Build on the Pi

```bash
git clone https://github.com/zrougamed/nyxd.git
cd nyxd
make build build-nyx
sudo install -m 755 bin/nyxd bin/nyx /usr/local/bin/
```

## Cross-compile on an x86_64 PC

```bash
make build-arm64
# copy bin/nyxd-arm64 to the Pi as /usr/local/bin/nyxd (rename if you like)
scp bin/nyxd-arm64 pi:/tmp/nyxd && ssh pi 'sudo install -m 755 /tmp/nyxd /usr/local/bin/nyxd'
```

Build the **nyx** client for `GOOS=linux GOARCH=arm64` the same way (add a Makefile target if you want both binaries in one shot).

## Optional: CNI plugins

Only if you set **`nyxd -net-driver=cni`**:

```bash
sudo make install-cni
```

## systemd

```bash
sudo cp nyxd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nyxd
```

The unit in this repo uses **`Type=notify`** but the daemon does not call `sd_notify` yet — if systemd complains, switch the unit to **`Type=simple`**. See comments in `nyxd.service`.

## Data directories

Default **`--base-dir=/var/lib/nyxd`**. Ensure the filesystem is persistent (SD card or USB disk), not a tiny tmpfs-only root.

## Next steps

- **[USAGE.md](USAGE.md)** — `nyx` commands and `curl` examples  
- **[openapi.yaml](openapi.yaml)** — HTTP API contract  
- **[networking.md](networking.md)** — native vs CNI, `nyx run` / stop behavior  
- **[qemu-alpine.md](qemu-alpine.md)** — try nyxd inside QEMU with Alpine 3.23 before touching bare metal  

## Security scanning (dev machine)

From the repo root (install [Trivy](https://aquasecurity.github.io/trivy/) and [Grype](https://github.com/anchore/grype) first):

```bash
make scan-trivy
make scan-grype
# or both:
make scan
```
