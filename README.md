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
2. Starts the child under `systemd-run --user --scope` with
   `MemoryMax=<memory>`, `CPUQuota=<cpus*100>%`, `RuntimeMaxSec=<ttl>`,
   `TasksMax=64`, `NoNewPrivileges=yes`, and `PrivateNetwork=yes` for
   `--network none`. For `--network localhost` the net namespace is still
   unshared, but loopback is brought up inside the sandbox, so the child
   can talk to itself on `127.0.0.1` and nothing else.
3. Inside that, `bwrap` with `--ro-bind /usr /usr`, a minimal `/etc`
   (`ld.so.cache`, `ssl`, `resolv.conf`, `hosts` when present), each input
   as `--ro-bind <input> /input/<n>`, `--bind <root>/<id>/out /out`,
   `--bind <root>/<id>/home /home`, `--tmpfs /tmp`, `--tmpfs /run`,
   `--proc /proc`, `--dev /dev`, `--unshare-pid`, `--unshare-net`,
   `--unshare-ipc`, `--unshare-uts`, `--die-with-parent`, `--new-session`.
   `/etc` and `/` are remounted read-only after the binds, so the child
   cannot create `/etc/lease-test` or write anywhere except `/out`,
   `/home`, `/tmp` and `/run`.
4. The child environment starts empty (`--clearenv`) plus
   `PATH=/usr/local/bin:/usr/bin:/bin`, `LEASE_ID`, `LEASE_OUT=/out`,
   `LEASE_INPUT_<n>=/input/<n>`, and each `--env KEY=VALUE`. The host
   environment is never passed through.
5. Stdout/stderr are captured to `<root>/<id>/stdout.log` and
   `stderr.log`, capped at 10 MB each. On exit the process group is
   killed, `lease.json` gains `ended`, `exit_code`, `signal`,
   `peak_memory_kb` (when available), and the sandbox `/out` is copied to
   the host `--out`.
6. With `--json` it prints one final JSON object; without it, it prints
   the child's stdout. One telemetry line goes to stderr per run.

```json
{"ok":true,"lease":"<id>","exit_code":0,"signal":"",
 "out":"/abs/out","stdout":"/abs/stdout.log","stderr":"/abs/stderr.log",
 "duration_ms":123,"timed_out":false}
```

Exit codes: `0` success, `1` the child exited non-zero, `2` usage or
input error, `3` tool failure (missing `bwrap`/`systemd-run`, a lease
that could not start, or a child killed by its TTL). Failures print one
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
   forbids. `lease` adds `--remount-ro /etc` and `--remount-ro /`
   after the binds (`/out`, `/home`, `/tmp`, `/run` stay writable
   because they are separate mounts).
6. **`PrivateNetwork=yes` is not loopback-only.** It gives the child no
   network at all, loopback down. For `--network localhost`, `lease`
   omits `PrivateNetwork`, keeps `bwrap --unshare-net`, and brings `lo`
   up inside the sandbox (`ip link set lo up`) before execing the tool.
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
9. **Tagging.** The release deliverable asks for a `v0.1.0` tag; the
   tag is created from the release workflow / a maintainer's `git tag`
   (see CHANGELOG). Binaries and `sha256sum.txt` are built by
   `make dist sha256` and uploaded by CI.

## Layout

```text
go.mod
cmd/lease/main.go
internal/sandbox/{run.go,bwrap.go,systemd.go,lease.go,gc.go,doctor.go,selftest.go}
schema/{request,response,error,telemetry}.json
fixtures/{tool-echo.sh,input.log,expected.json}
plugin.toml
README.md
LICENSE
CHANGELOG.md
Makefile
.github/workflows/ci.yml
```

## Known limitations

- `peak_memory_kb` is only available on the direct-`bwrap` path (from
  the launcher's rusage); under `systemd-run` the scope's cgroup is
  gone by the time the run ends, so it reports 0.
- `--network localhost` needs the `ip` binary under `/usr` inside the
  sandbox; without it, loopback stays down and the run still proceeds.
- `lease` enforces the TTL itself and uses `RuntimeMaxSec` as a
  backup; a child that ignores `SIGKILL` cannot exist, but a child
  that double-forks out of the process group is only reaped by `gc`.
