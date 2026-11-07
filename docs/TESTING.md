# lease — Testing

How to verify lease, what has been verified, and what still needs a
different host. Every claim in this file is tied to a command you can
re-run.

## Test environment used so far

- Ubuntu 24.04 container, systemd 255 as PID 1, Go 1.23.0, bubblewrap
  0.9.0, user namespaces enabled.
- `/tmp` is a 512 MB tmpfs and is usually ~97% full (other sessions
  download Go toolchains there). **Never download large files to `/tmp`,
  and never put lease roots on `/tmp` for large-output tests.** Use a
  directory under `~/workspace` instead. Go builds also need
  `TMPDIR` pointed somewhere with space (Go uses `$TMPDIR` for its
  `$WORK` directory): `TMPDIR=$HOME/workspace/lease-work/.tmp`.
- The VM has **no user systemd bus by default** and **no cgroup
  controllers delegated** (`/sys/fs/cgroup/cgroup.controllers` is empty).
  See "Starting a user bus in this VM" below for how to get one, and
  "What is still unverified" for what that means.

## Quick check

```sh
make check   # gofmt + go vet + go test + --self-test + doctor
```

## Full verification from a clean clone

```sh
git clone https://github.com/daxpublicfirst-arch/lease.git checkout
cd checkout
git rev-parse HEAD            # must match the head on GitHub (list_commits)

# Fresh toolchain (do not reuse another session's Go or caches):
# download go1.23.0.linux-amd64.tar.gz from go.dev into ~/workspace,
# extract it, then:
export PATH=$PWD/../go/bin:$PATH
export GOCACHE=$PWD/../.gocache GOPATH=$PWD/../.gopath
export TMPDIR=$PWD/../.tmp

gofmt -l cmd internal        # expect: empty
go vet ./...                 # expect: exit 0
go test -count=1 -v ./...    # expect: 13/13 pass (as of the 2026-11-08 follow-up)
make build && ./bin/lease --version   # expect: lease 0.1.0, static binary
./bin/lease doctor           # expect: exit 0
./bin/lease --self-test      # expect: 6/6 PASS
make dist sha256             # expect: exit 0
(cd dist && sha256sum -c sha256sum.txt)  # expect: 4/4 OK
```

## Starting a user bus in this VM

The VM's PID 1 is systemd, but no system D-Bus daemon runs and no user
manager is started. The sequence that works (as root):

```sh
mkdir -p /run/dbus
dbus-daemon --system --fork
systemctl start user-runtime-dir@0.service
systemctl start user@0.service
export XDG_RUNTIME_DIR=/run/user/0
export DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/0/bus
systemctl --user status   # expect: State: running
```

Without the system bus, `user-runtime-dir@0.service` fails with
"Failed to connect to system bus: No such file or directory" and
`user@0.service` fails its dependency. With the bus up, lease's
`systemdAvailable()` probe passes and every run takes the systemd path
(`systemd:true` in `lease.json`). The user manager does **not** survive
a VM restart; re-run the sequence after a reboot.

## Test matrix (what to run, what to expect)

| Test | Command | Expect |
|---|---|---|
| Static checks | `gofmt -l`, `go vet`, `go test` | clean, 13/13 pass |
| Doctor | `./bin/lease doctor` | exit 0; 7 checks; `resource-limits` warns on this VM |
| Self-test | `./bin/lease --self-test` | 6/6 PASS, ~3 s (fallback) or ~4 s (systemd path) |
| Fixture run | `run --tool ./fixtures/tool-echo.sh --input ./fixtures/input.log --json -- hello` | exit 0, stdout exactly `LEASE_OUT=/out`, `LEASE_INPUT_0=/input/0`, `args: hello` + log lines |
| Isolation | tool writes `/out/result.txt`, `/home/note.txt`, reads `/etc/passwd`, reads host home | exit 0; `result.txt` copied to `--out`; host files invisible; `/proc` empty |
| Multi-input | 2 inputs, first a directory | child `pwd=/input/0`, sees both inputs |
| Loopback | `--network localhost`, Python binds 127.0.0.1 and connects | `loopback-ok pong`, exit 0 |
| TTL | `sleep 10`, `--ttl 2` | killed at ~2.0 s, `timed_out:true`, exit 3 |
| gc | run `sleep 30`, `gc --older-than 0` after 1 s | lease removed, process gone |
| status | `status --json` | lists the lease, `alive:false` after exit |
| Envelope | `--request fixtures/request-example.json` (paths fixed up) | exit 0 |
| Error cases | missing tool / missing input / bad `--network` / unknown command / no `bwrap` in PATH | exit 2 / 2 / 2 / 2 / 3, exact error codes |
| Peak memory | Python allocates 200 MB, sleeps 2 s | `peak_memory_kb` ≈ 210 000, `peak_source: "proc-poll"` |
| Memory limit (this VM) | same hog, `--memory 50M` | exit 0 — **not enforced here** (no controller); `limits_enforced:false` + reason |
| Large input | 7.5 MB log through the fixture tool | exit 0, 400 003 lines in `stdout.log`, ~90 ms |
| Stdout cap | child prints 11 MB | `stdout.log` exactly 10 485 760 bytes, `stdout_truncated:true`, exit 0 |
| Input limit | sparse 201 MB file as input | `INPUT_TOO_LARGE`, exit 2 |
| Concurrent | 5 runs in parallel, same `--root` | 5/5 exit 0, 5 unique ids, `gc --older-than 0` removes all 5 |
| Log write failure | `--root` on a full disk | `OUTPUT_ERROR`, exit 3 (not `CHILD_EXIT` 141) |

## What has been verified (2026-11-08, head 591dbff)

Everything in the table above, from a clean clone, on both the fallback
path (no bus) and the systemd path (bus started as above). The systemd
path was verified end-to-end for the first time in the 2026-11-08
follow-up, after the user bus was started in the VM. That session found
and fixed a real bug: the scope properties `NoNewPrivileges` and
`PrivateNetwork` were rejected by systemd ("Unknown assignment"), so
every systemd-path run failed until they were removed (ADR-05). It also
found that `peak_memory_kb` reported the launcher's rusage instead of
the tool's (ADR-15), and that a full `/tmp` made lease blame the child
for a host disk problem (ADR-17).

## What is still unverified

These need a host this VM is not:

1. **Memory/CPU limit enforcement.** This VM's cgroup view has no
   memory or cpu controller, so `MemoryMax`/`CPUQuota` are accepted but
   not enforced (verified: a 200 MB allocation succeeded under
   `MemoryMax=50M`). On a normal desktop/server host with a user
   session, run the hog test with `--memory 50M` and expect the child
   to be OOM-killed with `CHILD_OOM` (exit 1) and `oom_killed:true`
   in `lease.json`. Also verify a CPU spinner under `--cpus 1` takes
   ~2x wall time for 2 s of CPU work.
2. **cgroup `memory.peak` path.** On a host with controllers, `peak_source`
   should be `"cgroup"` and the number should include page cache (larger
   than the poller's number for I/O-heavy tools).
3. **arm64 binary execution.** The arm64 binary is built and verified as
   a static ELF, but never executed here (no `qemu-aarch64`). Run it on
   an ARM host or under `qemu-aarch64-static`.
4. **200 MB input at the boundary and 10 MB stderr cap.** The 201 MB
   sparse-file rejection and the 10 MB stdout cap are verified; the
   stderr cap uses the same code path but has not been exercised with
   11 MB of stderr.
5. **Long-running / concurrent load.** 5 parallel runs are verified;
   50+ parallel runs and multi-hour TTLs are not.
6. **A host where `/proc` can be mounted.** The proc-mount fallback is
   verified here (empty `/proc`); the fresh-`/proc` path is only
   verified by unit test, not by an end-to-end run on a permissive host.
