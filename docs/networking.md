# nyxd networking

This document describes how the daemon picks a network implementation, how that maps to code, and how to run with **native** (default) or **CNI** plugins.

## Who chooses the driver?

**Only `nyxd` (the daemon)** selects the network backend via CLI flags. The **`nyx` client** does not set `-net-driver`; it talks to the control socket. After you change networking flags, **rebuild and restart `nyxd`** so the new binary and options are in effect.

## Default: native (no `/opt/cni/bin`)

By default, `nyxd` runs with:

```bash
sudo ./nyxd --log-level info
# equivalent explicit flag:
# sudo ./nyxd --net-driver=native --log-level info
```

On startup you should see a JSON log line similar to:

```json
{"msg":"network backend","driver":"native",...}
```

Then run a container:

```bash
sudo ./nyx pull nginx:alpine
sudo ./nyx run nginx:alpine
```

**Foreground vs detach:** by default, `nyx run` prints the JSON response, then **waits**; **Ctrl+C** (or SIGTERM) calls **`POST /v1/containers/{id}/stop`** so the workload is torn down (similar to `docker run` without `-d`). Use **`nyx run -d`** / **`--detach`** to exit immediately after start (old behavior). You can also run **`nyx stop <id>`** from another terminal.

Native networking is implemented under `internal/network/native/` (bridge, veth, IPAM, nftables-based port maps). It does **not** require the standard CNI plugin bundle under `/opt/cni/bin`.

## Optional: CNI plugins (`-net-driver=cni`)

To use the **exec-based** path that shells out to standard CNI plugins (`bridge`, `host-local`, etc.), start the daemon with:

```bash
sudo ./nyxd -net-driver=cni \
  -cni-bin-dir=/opt/cni/bin \
  -cni-conf-dir=/etc/cni/net.d \
  -network=nyx \
  --log-level info
```

Install plugins first, for example:

```bash
make install-cni
# or install containernetworking/plugins to /opt/cni/bin by your own process
```

You should then see:

```json
{"msg":"network backend","driver":"cni",...}
```

## Code layout: `network.Backend` and the supervisor

### `network.Backend`

The interface is defined in `internal/network/backend.go`:

- `EnsureNetwork()` — host-wide setup (bridge / CNI conflist, etc.)
- `Setup(ctx, containerID, netNSPath, ports)` — join the container netns, return allocated IP (or equivalent)
- `Teardown(ctx, containerID, netNSPath)` — tear down that container’s networking

Implementations:

| Implementation | Package / type | Notes |
|----------------|----------------|--------|
| Native | `internal/network/native` — `*native.Manager` | In-process; no CNI binaries on disk |
| CNI exec | `internal/network` — `*network.Manager` | Runs plugins from `-cni-bin-dir` |

Both satisfy `network.Backend` (see compile-time assertions in `native/manager.go` and `network/cni.go`).

### Supervisor

`internal/supervisor` holds a `network.Backend`, not a concrete `*network.Manager`. `supervisor.New(rt, ovl, net, baseDir, log, logColl, imgStore)` accepts whichever backend `cmd/nyxd` constructed (and an optional image store for post-restart re-adoption).

### `network.PortMapping`

`supervisor.ContainerSpec` uses `[]network.PortMapping`. The native stack imports and uses **`network.PortMapping`** as well, so port maps from the API/supervisor align with native setup without duplicate structs.

## systemd

The repo’s `nyxd.service` example runs with **`--net-driver=native`** and does not mount `/etc/cni` in `ReadWritePaths` (native mode does not need it). If you switch the unit to `-net-driver=cni`, add `/etc/cni` (and ensure `/opt/cni/bin` is available on the host) as appropriate for your layout.

## Related docs

- [Native network internals](native-network.md) — bridge, IPAM, nftables, kernel modules
- [Kernel requirements](kernel-requirements.md) — modules and sysctl
- [Roadmap](ROADMAP.md) — delivery checklist including `-net-driver`
