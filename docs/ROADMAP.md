# nyxd roadmap

Legend: `[x]` shipped in tree (still may need polish), `[ ]` not done, `[~]` partial / risky.

---

## Already in the tree (high level)

- [x] **OCI runtime shell-out** — `internal/runtime`: `crun` create/start/run (detach)/kill/delete/state/list, state JSON parsing, dedicated `--root` state dir.
- [x] **Supervisor skeleton** — `internal/supervisor`: `Start` / `Stop` / `Remove` / `Shutdown`, restart policies, overlay + CNI setup + `bundle.Generate` + `crun run --detach`, supervised restart loop with backoff.
- [x] **Image pull (Docker Hub–style)** — `internal/image`: `Store`, anonymous token auth, manifest (+ index) fetch, concurrent layer download with digest verify, atomic blob writes, `ParseRef`, `LoadImageMeta`, `LoadManifest`.
- [x] **CNI exec path** — `internal/network`: conflist generation, `EnsureNetwork`, `Setup`/`Teardown` via plugin exec, netns under `/run/nyxd/netns`, portable `detachUnmount` for teardown.
- [x] **Bundle / OCI config** — `internal/bundle`: `config.json` generation with default caps, masked paths, `noNewPrivileges`, cgroup resource mapping.
- [x] **Compose subset** — `internal/compose`: real YAML (`gopkg.in/yaml.v3`), image/restart validation, unknown `depends_on` detection, cycle detection, `TopologicalOrder` helper, default `no_new_privileges=true` when unset.
- [x] **Healthcheck library** — `internal/health`: exec (via `crun exec` + `CommandContext`), HTTP, TCP, retries, `onUnhealthy` callback hook (caller must wire policy).
- [x] **Logs** — `internal/logs`: JSONL append per container, `Tail` (reads whole file — see gaps).
- [x] **Telemetry types** — `internal/telemetry`: counters + optional Prometheus-ish HTTP handler (`ServeMetrics`) — **not called from daemon today**.
- [x] **Daemon entry** — `cmd/nyxd`: wiring for store, overlay, **network backend** (`-net-driver`), runtime, log collector, supervisor; graceful shutdown context (30s); root check; **Unix socket HTTP control API** (`-socket`, default `/run/nyxd/nyxd.sock`); `go.sum` present after `go mod tidy`.
- [x] **`nyx` CLI client** — `cmd/nyx`: `ping`, `version`, `pull`, `run`, `exec`; `make build-nyx` → `bin/nyx`.
- [x] **Unit tests** — compose parser, image ref parsing, native IPAM (Linux build); no end-to-end integration tests.
- [x] **Docs / packaging** — kernel requirements, native network notes, example service units (`Type=simple` in `packaging/nyxd.service`, `Type=notify` in repo `nyxd.service` without `sd_notify` yet).

---

## Runtime / crun

- [ ] **Zero-latency exit wait** — `WaitForExit` still polls `crun state` every 200ms; prefer `crun events --format json`, pidfd, or inotify on state dir where portable.
- [~] **Robust exit code** — prefers `exit_code` in state JSON when present, then `<root>/<id>/exit_code`, then a second raw JSON parse for alternate keys (`exitCode`, `exit_status`, …). Still no `crun delete` stdout fallback.
- [x] **`crun exec` for one-off commands** — `runtime.Runtime.Exec` + control route `POST /v1/containers/{id}/exec` + `nyx exec …`.

**Done / partial**

- [x] **Lifecycle via CLI** — create/start/run/kill/delete/state/list implemented around one `Runtime` type.
- [~] **WaitForExit** — works via polling; high latency vs event-driven.

---

## Supervisor

- [~] **Shutdown fairness** — each parallel `Stop` now wrapped in its own **45s** timeout (`Shutdown` context still shared); a hung `crun kill` no longer blocks others indefinitely, but there is no global “all must finish by T” budget beyond the caller’s `ctx`.
- [x] **Backoff jitter** — exponential backoff adds small random jitter (`math/rand/v2`) to reduce thundering herds.
- [ ] **`depends_on` start ordering** — compose validates deps + exposes `TopologicalOrder`, but **supervisor does not** start services in that order (map iteration / separate `Start` calls only).
- [ ] **Readiness vs “started”** — container considered live after `crun run` succeeds; no wait for init HTTP/TCP or compose `healthcheck` before declaring ready (health `Checker` exists but is not integrated in `supervisor.go`).
- [ ] **Unhealthy → restart** — no automatic policy wiring from `health.Checker` to `Stop`/`Restart` (callback exists, supervisor never passes it today).

**Done / partial**

- [x] **Restart policies** — always / on-failure / unless-stopped / never + `MaxRestarts` gate.
- [x] **Basic backoff** — between restart attempts (no jitter).
- [x] **`Healthcheck` on spec** — field on `ContainerSpec` for future wiring.

---

## Image puller

- [ ] **Private / credentialed registries** — only anonymous Docker Hub token flow; no `DOCKER_CONFIG`, no basic auth, no OAuth refresh helper.
- [ ] **Resume partial downloads** — interrupted layer fetch restarts full blob (temp file removed on failure paths).
- [ ] **Platform override** — manifest index handling hard-requires `linux/amd64` string in code; no `-platform` / GOARCH-aware selection for arm64 etc.
- [ ] **Garbage collection** — blobs never reclaimed when images removed; store grows monotonically.
- [~] **`fetchBlob` return path** — layer pulls use **`fetchBlobToDisk`** (stream + verify + rename, no full-blob `ReadFile` after write). Small config blobs still use `fetchBlob` which re-reads from disk (acceptable size).

**Done / partial**

- [x] **Public pull + verify** — digest verify on write, atomic rename, bounded concurrency, 4MiB cap on manifest **response** body read (not full layer in RAM during copy).
- [x] **`ParseRef` / `LoadImageMeta` / `LoadManifest`** — for tooling and unpack helpers.

---

## Overlay

- [ ] **Safe tree walk** — `processWhiteouts` uses `filepath.Walk` (follows symlinks); harden against malicious layers (manual walk / no symlink follow / max depth).
- [ ] **Layer deduplication** — same digest extracted once per image path; no cross-image shared extraction cache.
- [~] **`extractTar` via host `tar`** — pragmatic but not all OCI whiteout variants (e.g. `.wh..wh..plnk` hardlink whiteouts) guaranteed; pure-Go or container-aware extractor still TBD.

**Done / partial**

- [x] **overlayfs mount lifecycle (Linux)** — lower/upper/work/merged layout, `Remove` unmount + cleanup; non-Linux stub returns clear error.

---

## Native network (`internal/network/native`)

- **Operator guide:** [networking.md](networking.md) — `-net-driver`, `network.Backend`, `nyxd` vs `nyx`, systemd, CNI optional path.

### Design / internals

- [~] **`runNft` / `addPortMappings`** — uses `exec.Command` + `withTimeout` instead of `syscall.Exec` (daemon no longer loses the process). Rule syntax / nft availability may still fail at runtime; errors are logged.
- [x] **`ensureNftTable`** — initial table load uses `exec.Command("/usr/sbin/nft", "-f", file)` inside `sync.Once` (no `unix.Exec`).
- [x] **`withTimeout`** — used by `runNft` for each shell-out.
- [ ] **`portmapState`** — declared; `removePortMappings` is still a stub — teardown does not delete DNAT rules.
- [ ] **IPAM bounds** — `last = base + 0xFFFE` ignores real prefix length; breaks for subnets smaller than `/16` (allocate outside CIDR).
- [ ] **IPv6** — IPv4-only assumptions throughout bridge + NAT.

**Done / partial**

- [x] **Linux-only in-process bridge + netlink RTNETLINK** — veth, bridge attach, basic IPAM file backend, tests for allocator on Linux.
- [x] **`!linux` stub** — builds on macOS/CI without native stack.

---

## Compose parser

- [x] **Real YAML parsing** — `compose.Parse([]byte)` with `yaml.v3` (replaces old `parseYAML` stub narrative).
- [ ] **`depends_on` enforcement at runtime** — validated + `TopologicalOrder` exported; **supervisor / daemon do not consume it yet**.
- [ ] **Variable substitution** — no `${VAR}` / `.env` file interpolation.
- [ ] **Volume / bind mount model** — types may mention volumes; no mount wiring through to `bundle.Generate` from compose file today.

**Done / partial**

- [x] **Restart policy validation**, **cycle detection**, **unknown dependency errors**, **default `no_new_privileges`**.

---

## Health checks

- [ ] **Supervisor integration** — `internal/health` not started from `supervisor.startOnce`; no link from unhealthy status to restart/stop.
- [~] **Exec hang** — `checkExec` uses `exec.CommandContext` with timeout ctx (good baseline); still depends on crun honoring signals/cancellation.

**Done / partial**

- [x] **Checker implementation** — interval, timeout, retries, start period, HTTP/TCP/exec/`none`.

---

## Log collector

- [ ] **Rotation by size** — `Rotate` exists but no max-size trigger; growth unbounded.
- [ ] **`Tail` memory** — reads/decodes entire JSONL file then truncates to last *n* — unsafe for large logs.
- [ ] **Follow mode** — no `nyxd logs -f` / streaming API.
- [ ] **Alternate sinks** — JSONL to disk only; no syslog / journald forwarder interface.

**Done / partial**

- [x] **Stream from reader to JSONL file** — `Collector.Stream` with context cancellation.

---

## Telemetry

- [ ] **Wire-up** — `telemetry.New` / `ServeMetrics` never referenced from `cmd/nyxd` or supervisor; counters stay at zero.

**Done / partial**

- [x] **Package scaffold** — HTTP `/metrics` handler skeleton in `internal/telemetry`.

---

## `nyx` CLI & Unix control socket

**Yes:** `nyxd` starts an **HTTP server on a Unix domain socket** by default (`-socket=/run/nyxd/nyxd.sock`, override or set `-socket=""` to disable). The **`nyx`** binary is the thin client (`cmd/nyx`).

- [x] **Socket server** — `internal/control`: `GET /v1/ping`, `GET /v1/version`, `GET /v1/containers`, `POST /v1/images/pull`, `POST /v1/containers/run`, `POST /v1/containers/{id}/exec`.
- [x] **`nyx run`** — `POST /v1/containers/run`: resolves pulled image (`ResolvePulledImage`), builds `ContainerSpec`, **`supervisor.Start`** (overlay + CNI + bundle + `crun run --detach`). Optional `restart` policy in JSON.
- [ ] **Auth / TLS** — socket is world-group writable (`0660`); no peer cred check, no token yet (local trust model only).
- [ ] **Structured errors** — failed `exec` still begins `200` + stream body in some cases; tighten status codes and cap output size.

---

## `cmd/nyxd` / control plane

- [x] **Imports / build** — compiles; `go run ./cmd/nyxd` works.
- [~] **Subcommands on `nyxd` itself** — still no `nyxd pull` subcommand; use **`nyx pull`** against the socket, or the HTTP API. *(See **API, clients & UI** for OpenAPI, SDK samples, and UI.)*
- [x] **Control socket flag** — `-socket` (default `/run/nyxd/nyxd.sock`, `""` disables).
- [x] **Native network selection** — `-net-driver` (default **`native`**: in-process `internal/network/native`); **`cni`** uses exec plugins under `-cni-bin-dir`.
- [ ] **`systemd-notify`** — `nyxd.service` uses `Type=notify` but process never sends `READY=1` / reloading state; switch to `Type=simple` or implement sd_notify.

**Done / partial**

- [x] **Operational flags** — base dir, crun path, **`-net-driver`** (native|cni), CNI paths (cni mode), network name (cni mode), log level, version, socket.

---

## API, clients & UI

- [ ] **OpenAPI spec** — publish `openapi.yaml` (or JSON) for the control/daemon HTTP API: routes, schemas, auth, errors; use for codegen, docs, and CI contract checks once handlers exist.
- [ ] **SDK samples** — small runnable examples (e.g. Go, Python, shell+curl) that call the API for pull, run, status, logs; live under `examples/` or docs and stay in sync with the spec.
- [ ] **UI** — operator-facing web (or desktop) UI for host/node view, container lifecycle, compose stacks, log tail, and metrics; consumes the same API + optional WebSocket/SSE for streaming.

---

## Security

- [ ] **Default seccomp JSON artifact** — bundle supports hardening fields; no curated default profile shipped beside comments in older specs.
- [ ] **AppArmor** — no profile generation or integration.
- [ ] **Rootless** — requires root today; no user-namespace rootless path.

**Done / partial**

- [x] **Bundle defaults** — dropped caps, masked paths, `noNewPrivileges` in generated JSON where bundle applies.

---

## General / engineering

- [ ] **CI** — no `.github/workflows` in repo; add lint + `go test` + cross-compile (`GOOS=linux`).
- [ ] **Integration tests** — no crun-in-container tests; only targeted unit tests.
- [x] **`go.sum`** — committed / maintained via `go mod tidy` (verify in CI).

**Done / partial**

- [x] **Small module footprint** — stdlib + `yaml.v3` + `x/sys` + OCI spec packages as declared in `go.mod`.

---

## How to run the daemon and the `nyx` client

1. **Build** (Linux, as root for real networking/overlay):

   ```bash
   make build
   sudo ./bin/nyxd --log-level info
   ```

   or:

   ```bash
   sudo go run ./cmd/nyxd --log-level info
   ```

2. **Control API** — With defaults, the daemon listens on **`/run/nyxd/nyxd.sock`**. From another shell (root or user in group that can RW the socket):

   ```bash
   make build-nyx
   ./bin/nyx ping
   ./bin/nyx version
   ./bin/nyx pull nginx:alpine
   ./bin/nyx run nginx:alpine
   ```

3. **What you should see** — Daemon logs include **`network backend`** with **`driver":"native"`** (unless you set `-net-driver=cni`), then `daemon ready - awaiting workload` and `control API listening` when the socket bound. The process blocks until SIGINT/SIGTERM. See [networking.md](networking.md).

4. **Running a workload** — After **`nyx pull <ref>`**, **`nyx run <ref>`** calls **`POST /v1/containers/run`** (default restart `unless-stopped`). The daemon resolves local image metadata + layer blobs, then **`supervisor.Start`** builds overlay, **networking** (default in-process native), bundle, and **`crun run --detach`**. By default the **CLI waits**; **Ctrl+C** calls **`POST /v1/containers/{id}/stop`**. Use **`nyx run -d`** to exit immediately after start, or **`nyx stop <id>`** from another shell. Use **`nyx exec <id> -- …`** for one-off commands inside the container.

5. **Disable the socket** — `sudo ./bin/nyxd -socket="" …` if you do not want the control listener.

---

## Suggested priority (opinionated)

1. **nft / portmap follow-ups** — real rule handles + `removePortMappings`; fix IPAM `last` for non-/16 subnets.  
2. **Supervisor**: integrate **health** + **`TopologicalOrder`** + optional global shutdown budget.  
3. **Runtime**: `crun events` / pidfd instead of poll-only `WaitForExit`.  
4. **Registry**: auth + platform + GC + resumable layers.  
5. **OpenAPI spec** → **SDK samples** → **UI** (document `/v1/*` first).  
6. **CI + integration tests** on Linux runners with crun + CNI.

---

*Last reviewed against repository layout on 2026-05-17. Update checkboxes when merging features.*
