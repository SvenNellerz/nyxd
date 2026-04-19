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
- [x] **Daemon entry** — `cmd/nyxd`: wiring for store, overlay, CNI manager, runtime, log collector, supervisor; graceful shutdown context (30s); root check; `go.sum` present after `go mod tidy`.
- [x] **Unit tests** — compose parser, image ref parsing, native IPAM (Linux build); no end-to-end integration tests.
- [x] **Docs / packaging** — kernel requirements, native network notes, example service units (`Type=simple` in `packaging/nyxd.service`, `Type=notify` in repo `nyxd.service` without `sd_notify` yet).

---

## Runtime / crun

- [ ] **Zero-latency exit wait** — `WaitForExit` polls `crun state` every 200ms; prefer `crun events --format json`, pidfd, or inotify on state dir where portable.
- [ ] **Robust exit code** — `exitCode` reads `<rootDir>/<id>/exit_code`; path/layout may differ across crun versions; fall back to `crun delete` output or state JSON if missing.
- [ ] **`crun exec` for one-off commands** — no first-class API for `nyxd exec …` / debug shells (health package uses exec for checks only).

**Done / partial**

- [x] **Lifecycle via CLI** — create/start/run/kill/delete/state/list implemented around one `Runtime` type.
- [~] **WaitForExit** — works via polling; high latency vs event-driven.

---

## Supervisor

- [ ] **Shutdown fairness** — `Shutdown` stops containers concurrently but each `Stop` can block; no global deadline beyond caller’s context; one hung `crun kill` can stall overall shutdown.
- [ ] **Backoff jitter** — `backoff` is deterministic (`attempt² × 100ms` capped); add random jitter to avoid synchronized restarts.
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
- [~] **`fetchBlob` return path** — streams to disk with `io.CopyBuffer`, but **returns `os.ReadFile(dest)`**, loading the full blob into memory for callers that use the return value (safe for small config; wasteful if misused for layers — `pullLayers` discards return but still pays read-after-write today).

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

- [ ] **`runNft` / `addPortMappings`** — builds `nft` CLI strings but calls `syscall.Exec` (replaces process — wrong for a library); rules never applied correctly from long-running daemon.
- [ ] **`ensureNftTable`** — `unix.Exec` inside `sync.Once` replaces process; must use `exec.Command` / netlink nft API instead.
- [ ] **`withTimeout`** — helper exists but unused in nft path.
- [ ] **`portmapState`** — declared; `removePortMappings` is effectively empty — teardown does not delete DNAT rules.
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

## `cmd/nyxd` / control plane

- [x] **Imports / build** — single `time` import; compiles; `go run ./cmd/nyxd` or `go run ./cmd/nyxd/main.go` works for the **daemon-only** binary.
- [ ] **Subcommands / API** — no `nyxd image pull`, `nyxd run`, or Unix socket / gRPC; second terminal running `./nyxd image pull` is a **different binary or fork** — upstream `cmd/nyxd` only parses global flags and blocks on signal. Use `go doc` / README `nyxd image pull` once CLI exists, or call `image.Store.Pull` from a small tool. *(See **API, clients & UI** for OpenAPI, SDK samples, and UI.)*
- [ ] **Native network selection** — daemon always constructs **CNI** `network.Manager`; native manager not selectable via flag.
- [ ] **`systemd-notify`** — `nyxd.service` uses `Type=notify` but process never sends `READY=1` / reloading state; switch to `Type=simple` or implement sd_notify.

**Done / partial**

- [x] **Operational flags** — base dir, crun path, CNI paths, network name, log level, version.

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

## How to run the daemon today (and why `image pull` looks odd)

1. **Build** (Linux, as root for real networking/overlay):

   ```bash
   make build
   sudo ./bin/nyxd --log-level info
   ```

   or:

   ```bash
   sudo go run ./cmd/nyxd --log-level info
   ```

2. **What you should see** — Logs like `daemon ready - awaiting workload` then the process **blocks** until SIGINT/SIGTERM. There is **no** built-in subcommand in `cmd/nyxd` to pull or run images yet; workload wiring (`supervisor.Start`, compose loader, HTTP API) is still TODO.

3. **Pulling images** — Use the Go API (`image.Store.Pull`) from code/tests, or add a thin CLI wrapper package, or wait for roadmap items above. Running `./nyxd image pull …` only works if you built a **different** `main` that implements subcommands (not this repo’s `cmd/nyxd` alone).

4. **After pull (future / custom glue)** — You still need: blob paths → overlay prepare → `bundle.Generate` → `supervisor.Start` / `runtime.Run`. The roadmap items track making that path first-class.

---

## Suggested priority (opinionated)

1. Fix **native nft** path (`Exec` → `exec.Command`) or hide feature flag until safe; same for portmap teardown + IPAM `last` calculation.  
2. **CLI / socket API** — single binary with `serve`, `pull`, `run`, `logs`, or minimal HTTP control plane.  
3. **Supervisor**: integrate **health** + **`TopologicalOrder`** + shutdown deadlines.  
4. **Runtime**: `crun events` / better exit wait + `crun exec` surface for ops.  
5. **Registry**: auth + platform + GC + resumable layers.  
6. **CI + integration tests** on Linux runners with crun + CNI.  
7. **OpenAPI spec** → **SDK samples** → **UI** (after the control HTTP API is stable enough to version).

---

*Last reviewed against repository layout on 2026-05-16. Update checkboxes when merging features.*
