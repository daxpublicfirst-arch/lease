# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Fixed

- **systemd scope properties (critical).** `NoNewPrivileges=yes` and
  `PrivateNetwork=yes` are rejected by systemd for scopes ("Unknown
  assignment"), so every run on a host that actually has a user bus
  failed. Both are removed from the scope invocation; the isolation
  they were meant to provide already comes from bwrap (`--unshare-net`
  for network, the user-namespace uid mapping for setuid). `systemd-run`
  is now also passed `--quiet`, so its "Running as unit: ..." line no
  longer pollutes the child's stderr log.
- **Peak memory reported the wrong process.** `peak_memory_kb` used to
  be the launcher's rusage (bwrap's parent), not the tool's. It is now
  the scope cgroup's `memory.peak` when the cgroup is visible, else the
  highest `VmHWM` sampled from the tool process tree every 25 ms, and
  `lease.json` records which source was used (`peak_source`).
- **Log write failures blamed the child.** A full disk under `--root`
  made the child die with SIGPIPE (exit 141) and lease reported
  `CHILD_EXIT`. Log write errors are now reported as `OUTPUT_ERROR`
  (exit 3).

### Added

- `lease.json` now records `limits_enforced` (+ `limits_reason`),
  `peak_source`, `oom_killed`, `stdout_truncated` and
  `stderr_truncated`.
- New error code `CHILD_OOM`: the child was killed by the kernel OOM
  killer inside its cgroup (detected via `memory.events`).
- `doctor` gains a `resource-limits` check: it reports whether
  `MemoryMax`/`CPUQuota` will actually be enforced on this host, or
  only accepted and silently ignored.
- `make check`: the full local gate (gofmt, vet, unit tests,
  `--self-test`, `doctor`).
- `docs/`: `DESIGN.md`, `ARCHITECTURE.md`, `DECISIONS.md` (19 ADRs),
  `TESTING.md` — the reasoning behind every decision, so a reader of a
  fresh clone does not have to ask the maintainer.

## [0.1.0] - 2026-11-08

### Added

- Initial release of `lease`: run one CLI tool in a disposable sandbox.
- Commands: `run`, `gc`, `status`, `doctor`, `version`, plus `--self-test` and `--json`.
- Sandboxing via `bwrap` inside `systemd-run --user --scope` (with a direct-`bwrap`
  fallback when no user systemd bus is available), with read-only `/usr`, a
  minimal `/etc`, read-only inputs at `/input/<n>`, writable `/out` and `/home`,
  no network by default and loopback-only with `--network localhost`.
- TTL enforcement, 10 MB stdout/stderr caps, `lease.json` per run, telemetry
  line on stderr, deterministic JSON output and documented exit codes.
- `plugin.toml` (`arabica-plugin-v1`), JSON schemas in `schema/`, fixtures in
  `fixtures/`, `Makefile`, and CI that runs unit tests, `--self-test`, and `doctor`.
