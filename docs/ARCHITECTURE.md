# lease — Architecture

## Layout

```
go.mod                    Go module github.com/daxpublicfirst-arch/lease
cmd/lease/main.go         CLI entry: flag parsing, --request envelope, exit codes
internal/sandbox/
  lease.go                Lease record, errors, cappedWriter, copyDir
  run.go                  Run(): validation, lease setup, execute(), result
  bwrap.go                buildBwrapArgs: the sandbox filesystem + namespaces
  systemd.go              systemd-run wrapper, cgroup reads, limitsEnforced()
  peak.go                 peak memory poller (/proc VmHWM sampling)
  gc.go                   GC(): remove old leases, kill live ones
  doctor.go               Doctor(): host capability checks
  selftest.go             --self-test: 6 isolation proofs
plugin.toml               Arabica plugin v1 manifest
schema/*.json             request / response / error / telemetry JSON schemas
fixtures/                 tool-echo.sh, input.log, request-example.json
docs/                     DESIGN.md, ARCHITECTURE.md, DECISIONS.md, TESTING.md
```

## Data flow of one `lease run`

```
CLI flags / --request envelope
        |
        v
Run() — validate (TTL, CPUs, network, tool exists, inputs exist,
        total input <= 200 MB, bwrap + systemd-run in PATH)
        |
        v
create <root>/<id>/  (out/, home/, lease.json with start time)
        |
        v
systemdAvailable()? ---- yes ----> systemd-run --user --scope --quiet
        |                              -p MemoryMax= --memory
        |                              -p CPUQuota= --cpus%
        |                              -p RuntimeMaxSec= --ttl
        |                              -p TasksMax=64
        |                              -- bwrap ...
        +---- no -----> bwrap ...  (direct, lease-enforced TTL only)
        |
        v
bwrap: new user/pid/mount/ipc/uts/net namespaces
        |
        v
peak poller starts (samples VmHWM of the tool tree every 25 ms)
        |
        v
cmd.Wait() — exit code / signal / timed_out
        |
        v
peak: cgroup memory.peak if the scope cgroup is visible,
      else the poller's sampled max  (peak_source records which)
OOM:  memory.events oom_kill > 0  =>  CHILD_OOM
        |
        v
copy <root>/<id>/out  ->  host --out
        |
        v
one JSON result on stdout, one telemetry line on stderr, exit 0/1/2/3
```

The launcher's own TTL (a `context.WithTimeout` around the outer
command) is the primary timeout; systemd's `RuntimeMaxSec` is the backup.
A run that is killed by either is reported as `timed_out=true` with
`TIMED_OUT` and exit 3.

## The sandbox filesystem (what bwrap builds)

Order matters: bwrap applies filesystem options in argument order, so
`--ro-bind <tool> /tool` must come before the final `--remount-ro /`
(see ADR-11).

| Inside path | Source | Writable? |
|---|---|---|
| `/usr` | host `/usr`, read-only bind | no |
| `/bin`, `/lib`, `/lib64` | symlinks to `/usr/bin`, `/usr/lib`, `/usr/lib64` (they are symlinks on the host, so they cannot be bound separately) | no |
| `/tool` | the `--tool` file, read-only bind | no |
| `/input/<n>` | each `--input` file or directory, read-only bind | no |
| `/etc` | fresh tmpfs with only `ld.so.cache`, `ssl`, `resolv.conf`, `hosts` bound in, then remounted read-only | no |
| `/out` | `<root>/<id>/out` | **yes** |
| `/home` | `<root>/<id>/home` | **yes** |
| `/proc` | fresh proc mount, or an empty directory when the host forbids a fresh proc mount (see ADR-09) | no |
| `/dev` | `--dev /dev` (minimal devices) | no |
| `/` | remounted read-only last | no |

The child runs as uid 0 / gid 0 inside the user namespace
(`--unshare-user --uid 0 --gid 0`), which is what makes loopback setup
and the read-only root work without host root. The environment is wiped
(`--clearenv`) and rebuilt from `--env` plus `LEASE_OUT`, `LEASE_INPUT_<n>`
and a minimal `PATH`/`HOME`.

If the first input is a directory, bwrap `--chdir /input/0` makes the
tool start inside it. A non-executable tool (e.g. a script stored as
0644) is run via `/bin/sh` instead of failing with `EACCES`.

Network modes:

- `none`: `--unshare-net`, no interfaces at all.
- `localhost`: `--unshare-net` plus a wrapper that runs
  `ip link set lo up` before the tool, so loopback works but nothing
  else exists. (Loopback setup needs the user namespace + uid 0; see
  ADR-10.)

## The lease record

`lease.json` is written three times: at creation (so a crash still leaves
a record), after the launcher starts (PID/PGID, systemd choice), and at
the end (exit code, signal, timed_out, peak, limits, truncation flags).
Fields that are only known after the run use `omitempty`, except
`limits_enforced`, which is always present so a reader can never mistake
"unknown" for "enforced".

## Process model

```
lease (Go)
  └─ systemd-run --user --scope --quiet --unit=lease-<id>   [systemd path]
  │    └─ bwrap
  │         └─ tool (pid 1 inside the sandbox's pid namespace)
  └─ bwrap                                                [fallback path]
       └─ tool
```

lease waits on the outer process (systemd-run or bwrap) and kills the
whole process group (`Setpgid` + `KillGroup`) after the run, so no child
survives. The peak poller walks `/proc` to find descendants of the outer
process; it does not need to know the tool's host PID in advance.

## Where limits live

| Limit | Mechanism | Enforced when |
|---|---|---|
| Time (`--ttl`) | lease context + `RuntimeMaxSec` | always (lease) / systemd path |
| Memory (`--memory`) | `MemoryMax` (cgroup) | only when the user cgroup has the memory controller delegated |
| CPU (`--cpus`) | `CPUQuota` (cgroup) | only when the user cgroup has the cpu controller delegated |
| Processes | `TasksMax=64` (cgroup) | systemd path, controller permitting |
| Input size (200 MB) | checked before the run | always |
| Output size (10 MB/log) | cappedWriter truncation | always |

`limits_enforced` in `lease.json` and the `resource-limits` doctor check
report the memory/CPU rows honestly. See ADR-14.

## Concurrency

Runs are independent: each lease gets a random 32-hex id and its own
directory, so concurrent runs in the same `--root` do not collide.
`gc` and `status` only read `lease.json` files; they never touch a lease
that is still starting.
