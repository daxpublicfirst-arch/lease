# lease

Run one CLI tool in a disposable sandbox with a time limit and a cleanup.

`lease` does not build a new isolation layer. It uses `bwrap` inside
`systemd-run --user --scope`, gives the child a private directory, a
read-only root, only the named inputs (read-only), only the output
(writable), no network by default, a time limit, and a cleanup via `gc`.

It is harness-independent: it knows nothing about any harness, ticket,
product, or user. The harness wraps it with a thin adapter that calls this
CLI with explicit paths and reads JSON. No model calls, no LLM SDK, no API
keys, no prompt files, no network from `lease` itself. No daemon, no root.

- Version: `0.1.0`
- Protocol: `arabica-plugin-v1` (see `plugin.toml`)
- License: MIT

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md) — goals, non-goals, threat model, contracts.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — layout, data flow, sandbox filesystem, process model.
- [docs/DECISIONS.md](docs/DECISIONS.md) — the reasoning behind every decision (19 ADRs), with alternatives, trade-offs, and sources.
- [docs/TESTING.md](docs/TESTING.md) — how to verify lease, what has been verified, and what still needs a different host.

If you are reading a fresh clone and wondering *why* something is built
the way it is, start with `docs/DECISIONS.md`.

## Example

```sh
# Build
make build

# Run a tool on a log file, sandboxed, with JSON output
./bin/lease run --tool ./fixtures/tool-echo.sh --input ./fixtures/input.log \
  --out ./out --ttl 60 --network none --json -- hello

# The same run through the adapter envelope (paths in the file are examples)
./bin/lease --request ./fixtures/request-example.json --json

# Clean up leases older than an hour, list leases, check the host
./bin/lease gc --root /tmp/arabica-lease --older-than 3600
./bin/lease status --root /tmp/arabica-lease --json
./bin/lease doctor
./bin/lease --self-test
./bin/lease --version   # lease 0.1.0
```

`fixtures/request-example.json` is shipped as an example; an envelope looks like this:

```json
{"protocol":"arabica-plugin-v1","command":"run",
 "args":{"tool":"/abs/bin/tool","input":["/abs/log"],
         "out":"/abs/out","root":"/abs/leases","ttl":60,
         "network":"none","argv":["reduce","--kind","vitest","--log","/input/0","--out","/out","--json"]}}
```

## What `run` does

1. Creates `<root>/<id>` with mode `0700` (`<id>` is a random 32-char hex
   string), plus `<root>/<id>/out`, `<root>/<id>/home`, and
   `<root>/<id>/lease.json` (tool, inputs, out, pid, start, ttl, limits,
   network).
2. Starts the child under `systemd-run --user --scope --quiet` with
   `MemoryMax=<memory>`, `CPUQuota=<cpus*100>%`, `RuntimeMaxSec=<ttl>`,
   and `TasksMax=64`. Only resource-control properties are valid on a
   scope: `NoNewPrivileges=yes` and `PrivateNetwork=yes` are rejected
   by systemd with "Unknown assignment" (see research note 13), and the
   isolation they were meant to provide already comes from bwrap.
   For `--network localhost` the net namespace is still unshared, but
   loopback is brought up inside the sandbox, so the child can talk to
   itself on `127.0.0.1` and nothing else.
3. Inside that, `bwrap` with `--unshare-user --uid 0 --gid 0` (portable
   rootless mode), `--ro-bind /usr /usr`, a minimal `/etc` (tmpfs with
   `ld.so.cache`, `ssl`, `resolv.conf`, `hosts` when present), each input
   as `--ro-bind <input> /input/<n>`, the tool as `--ro-bind <tool> /tool`,
   `--bind <root>/<id>/out /out`, `--bind <root>/<id>/home /home`,
   `--tmpfs /tmp`, `--tmpfs /run`, `--proc /proc` (or an empty `/proc`
   dir where mounting one is not permitted — see below), `--dev /dev`,
   `--unshare-pid`, `--unshare-net`, `--unshare-ipc`, `--unshare-uts`,
   `--die-with-parent`, `--new-session`.
   `/etc` and `/` are remounted read-only after the binds, so the child
   cannot create `/etc/lease-test` or write anywhere except `/out`,
   `/home`, `/tmp` and `/run`.
4. The child environment starts empty (`--clearenv`) plus
   `PATH=/usr/local/bin:/usr/bin:/bin`, `LEASE_ID`, `LEASE_OUT=/out`,
   `LEASE_INPUT_<n>=/input/<n>`, and each `--env KEY=VALUE`. The host
   environment is never passed through.
5. Stdout/stderr are captured to `<root>/<id>/stdout.log` and
   `stderr.log`, capped at 10 MB each (the cap is recorded as
   `stdout_truncated`/`stderr_truncated` in `lease.json`). On exit the
   process group is killed, `lease.json` gains `ended`, `exit_code`,
   `signal`, `peak_memory_kb` and `peak_source`, `limits_enforced` (+
   `limits_reason`), `oom_killed` when applicable, and the sandbox
   `/out` is copied to the host `--out`.
6. With `--json` it prints one final JSON object; without it, it prints
   the child's stdout. One telemetry line goes to stderr per run.

```json
{"ok":true,"lease":"<id>","exit_code":0,"signal":"",
 "out":"/abs/out","stdout":"/abs/stdout.log","stderr":"/abs/stderr.log",
 "duration_ms":123,"timed_out":false}
```

Exit codes: `0` success, `1` the child exited non-zero (or was
OOM-killed, `CHILD_OOM`), `2` usage or input error, `3` tool failure
(missing `bwrap`/`systemd-run`, a lease that could not start, a child
killed by its TTL, or a log that could not be written). Failures print one
JSON object: `{"ok":false,"error":{"code":"...","blocked":"...","fix":"...","if_that_fails":"..."}}`.

## What the child can see

- `/input/<n>` for each `--input`, read-only.
- `/out` and `/home`, writable.
- `/usr`, minimal `/etc`, read-only.
- No host `/home`, no `~/.ssh`, no `~/.config`, no `.env`, no Docker
  socket, no `/var/run`, no network (or loopback only).

If a tool needs a file, pass it as `--input`; the bind set is never
widened for one tool.

## `gc` and `status`

```sh
lease gc --root <dir> --older-than <seconds> [--json]   # one line per removed lease; exit 0 even if none
lease status --root <dir> [--json]                      # id, tool, pid, start, ttl, network, out, alive
```

`gc` kills a still-running lease's process group before removing its
directory, and never removes a lease younger than the age.

## `--self-test`

Runs the six security tests in `fixtures/` against temporary roots and
inputs under the test's own temp dir — never the real home. It exits 0
only when every expected result matches:

1. Writing `/etc/lease-test` fails.
2. Reading the host user's `~/.ssh/id_ed25519` fails.
3. Opening a TCP connection fails with `--network none`.
4. Sleeping longer than `--ttl` is killed and reported `timed_out`.
5. `gc` removes the directory and leaves no process.
6. The child sees `LEASE_OUT=/out` and `LEASE_INPUT_0=/input/0`.

## Setup (clean checkout)

```sh
# Tools needed: Go 1.23+, bubblewrap, systemd (systemd-run)
# Ubuntu/Debian:
sudo apt-get install -y bubblewrap
# Go: https://go.dev/dl/ (or your package manager)

make build        # bin/lease
make check        # full local gate: gofmt + vet + unit tests + self-test + doctor
make test         # unit tests (no sandbox needed)
make vet          # go vet + gofmt check
make self-test    # full sandbox security tests
make doctor       # ./bin/lease doctor
make dist sha256  # dist/lease-0.1.0-linux-{amd64,arm64}.tar.gz + dist/sha256sum.txt
```

## Research notes: where the task prompt was wrong

The task prompt was a draft; these are the corrections applied, each
verified against the live host (Ubuntu 24.04, systemd 255) rather than
assumed:

1. **Repo.** The prompt named no repo; the fixed target is
   `daxpublicfirst-arch/lease`. The layout in the prompt (`go.mod`,
   `cmd/lease/main.go`, `internal/sandbox/…`) is implemented at the repo
   root, and `plugin.toml`'s `source` is the real repo URL.
2. **`/bin`, `/lib`, `/lib64` are symlinks.** On modern distros they point
   at `/usr/bin`, `/usr/lib`, `/usr/lib64`. Binding them separately (as
   the prompt listed) is wrong. `lease` binds `/usr` once and recreates
   the three symlinks inside the sandbox.
3. **The prompt never binds `--tool`.** With only the listed binds, the
   child could not exec the tool at all. `lease` binds the tool
   read-only at `/tool` and execs that.
4. **`RuntimeMax`, not `RuntimeMaxSec`.** The systemd property is
   `RuntimeMaxSec=<ttl>s`. The prompt's `RuntimeMax=<ttl>` is not a
   valid property.
5. **`/etc` was writable.** The prompt creates `/etc` with `--dir` and
   binds files into it, but a plain `--dir` is writable, so the child
   could create `/etc/lease-test` — the very thing security test 1
   forbids. `lease` mounts a tmpfs on `/etc`, binds the minimal files
   into it, and `--remount-ro /etc` (a plain `--dir` cannot be
   remounted at all — bwrap fails with "Unable to find /etc in mount
   table").
6. **`PrivateNetwork=yes` is not loopback-only — and not valid on a
   scope at all.** It gives the child no network at all, loopback down.
   Worse, systemd rejects it on scopes ("Unknown assignment", see note
   13). For `--network localhost`, `lease` omits it, keeps
   `bwrap --unshare-net`, and brings `lo` up inside the sandbox
   (`ip link set lo up`) before execing the tool.
7. **No user systemd bus in this cloud VM.** `systemd-run --user`
   fails here (`systemctl --user`: "No medium found") even though
   systemd is PID 1 and `systemd-run` exists. `lease` probes
   `systemd-run --user --scope` first; when it cannot start, it runs
   `bwrap` directly under the lease's own process group with the
   lease-enforced TTL, and records `systemd: false` plus the reason in
   `lease.json`. `doctor` reports this as a warning, not a failure —
   the binary being present is the requirement; the bus is
   environment-dependent.
8. **`--out` vs the lease `out`.** The prompt both creates
   `<root>/<id>/out` for the child and reports `--out` as the result
   path. `lease` treats `<root>/<id>/out` as the sandbox `/out` and
   copies it to the host `--out` after the run, so both are true.
9. **Tagging / CI.** The release deliverable asks for a `v0.1.0` tag
   and CI. The Muse GitHub App cannot create tags or releases (no such
   tool in its catalogue) and cannot write `.github/workflows/ci.yml`
   (403 — it lacks the `workflows` permission), so the CI file lives
   in the local checkout only. Per the maintainer's preference, no
   browser / web-UI step is used for the tag or release either — they
   are out of scope unless explicitly asked for. Binaries and
   `sha256sum.txt` are built by `make dist sha256` and uploaded by CI
   once the workflow file is added from a maintainer checkout.
10. **Loopback needs a user namespace.** Without `--unshare-user`,
    bwrap's loopback setup fails in this VM ("Failed RTM_NEWADDR: No
    child processes") because the container lacks `CAP_NET_ADMIN`.
    `lease` always uses `--unshare-user --uid 0 --gid 0`.
11. **A fresh `/proc` cannot be mounted in this VM.** Even plain
    `unshare --user --map-root-user --pid --mount-proc` fails with
    EPERM. `lease` first tries `--proc /proc` and, when bwrap fails
    with "Can't mount proc", retries once with an empty `/proc`
    directory (recorded as `proc_fallback` in `lease.json`). The child
    sees no host processes either way.
12. **Bind order matters, and so does the exec bit.** The `--ro-bind
    <tool> /tool` must come before `--remount-ro /`, or bwrap cannot
    create the `/tool` mount point ("Read-only file system"). And a
    tool script checked in without the exec bit (the GitHub API stores
    files as 0644) would fail with "Permission denied", so `lease`
    runs non-executable tools under `/bin/sh`.
13. **Scope properties: `NoNewPrivileges` and `PrivateNetwork` broke
    every systemd-path run.** The first build was only ever tested on
    a VM without a user bus, so every run took the direct-`bwrap`
    fallback and the systemd properties were never executed. On
    2026-11-08 a user bus was started in the VM (`dbus-daemon --system`
    + `systemctl start user@0.service`) and the systemd path ran for
    the first time — every run failed with "Unknown assignment:
    NoNewPrivileges=yes". Scopes only accept resource-control
    properties; a scope does not exec anything, so `systemd.exec`
    properties cannot apply to it. Both were removed; the isolation
    they were meant to provide already comes from bwrap (see note 6
    and `docs/DECISIONS.md` ADR-05). The same session found that
    `systemd-run` prints "Running as unit: ..." to stderr, which
    polluted the child's stderr log — `--quiet` is now passed.
14. **Accepting a limit is not enforcing it.** `systemd-run` happily
    accepts `MemoryMax=50M` on a host whose cgroup view has no memory
    controller — and the limit then does nothing (verified: a 200 MB
    allocation succeeded under `MemoryMax=50M` in this VM). `lease`
    now checks whether the user manager's cgroup has the `memory.max`
    / `cpu.max` control files and records `limits_enforced` +
    `limits_reason` in `lease.json` and a `resource-limits` check in
    `doctor`. `prlimit` was considered as a fallback enforcer and
    rejected: `RLIMIT_AS` counts virtual address space and breaks Go
    tools that reserve large virtual arenas.
15. **Peak memory reported the wrong process.** The first version
    reported the launcher's rusage — bwrap's parent, not the tool.
    `lease` now reads the scope cgroup's `memory.peak` when the cgroup
    is visible, else samples the tool tree's `VmHWM` from `/proc`
    every 25 ms, and records which source was used (`peak_source`).
    Verified: a tool allocating 200 MB reported ~215 000 KB.
    A child OOM-killed inside its cgroup is now reported as the
    distinct error `CHILD_OOM` instead of a bare `CHILD_EXIT`.
16. **A full disk blamed the child.** When `/tmp` (a 512 MB tmpfs,
    97% full) ran out of space mid-run, the log write failed, the
    child died with SIGPIPE (exit 141), and `lease` reported
    `CHILD_EXIT`. Log write errors are now reported as `OUTPUT_ERROR`
    (exit 3). Keep lease roots off a nearly-full `/tmp` for large
    outputs.

## Layout

```text
go.mod
cmd/lease/main.go
internal/sandbox/{run.go,bwrap.go,systemd.go,peak.go,lease.go,gc.go,doctor.go,selftest.go}
schema/{request,response,error,telemetry}.json
fixtures/{tool-echo.sh,input.log,expected.json}
docs/{DESIGN,ARCHITECTURE,DECISIONS,TESTING}.md
plugin.toml
README.md
LICENSE
CHANGELOG.md
Makefile
.github/workflows/ci.yml
```

## Known limitations

- **Resource limits are only enforced where the kernel can enforce
  them.** `lease` sets `MemoryMax`/`CPUQuota` on every systemd-path
  run, but on hosts without cgroup controllers delegated to the user
  manager (containers included) the kernel ignores them. `lease.json`
  says so (`limits_enforced:false` + reason) and `doctor` reports it
  — see research note 14. Filesystem/network isolation and the TTL
  still apply everywhere.
- `peak_memory_kb` is the cgroup `memory.peak` when the scope cgroup is
  visible, else the highest single process's `VmHWM` sampled every
  25 ms. The poller can miss a tool that allocates and exits within one
  sample interval, and the cgroup number counts page cache.
- `--network localhost` needs the `ip` binary under `/usr` inside the
  sandbox; without it, loopback stays down and the run still proceeds.
- `lease` enforces the TTL itself and uses `RuntimeMaxSec` as a
  backup; a child that ignores `SIGKILL` cannot exist, but a child
  that double-forks out of the process group is only reaped by `gc`.
