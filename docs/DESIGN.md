# lease — Design

`lease` is a standalone Arabica plugin (v1) CLI that runs one untrusted CLI
tool inside a disposable sandbox and reports what happened. This document
states the goals, the threat model, and the contracts. The implementation
layout is in [ARCHITECTURE.md](ARCHITECTURE.md); the reasoning behind each
decision is in [DECISIONS.md](DECISIONS.md); how to verify all of this is
in [TESTING.md](TESTING.md).

## What lease is for

An agent (or a human) wants to run a tool it does not fully trust — a
downloaded script, a generated helper, a log parser — and get its output
back without letting it read the rest of the machine, phone home, or run
forever. `lease` gives that tool a private, throwaway room:

- it can read only the files explicitly handed to it (`--input`, mounted
  read-only at `/input/<n>`)
- it can write only `/out` (copied to the host `--out` afterwards) and
  `/home` (a scratch home, discarded with the lease)
- it has no network by default; `--network localhost` gives it loopback
  only, so it can talk to a server it started itself and nothing else
- it is killed after `--ttl` seconds
- everything it did is recorded in `<root>/<id>/lease.json`, and old
  leases are swept by `lease gc`

## Goals

1. **Isolation by default.** A tool that tries to read the host home, the
   host SSH keys, `/etc/passwd`, or the network must fail, and the failure
   must be observable (exit code, log), not silent.
2. **Deterministic reporting.** Every run produces one JSON result on
   stdout (with `--json`) and one telemetry line on stderr, with a stable
   shape and a small set of exit codes (0/1/2/3). A harness must be able
   to tell "the tool failed" from "the user typed a bad flag" from "the
   sandbox itself is broken" without parsing free text.
3. **Honest limits.** Resource limits are only claimed when the kernel
   will really enforce them. When they cannot be enforced, lease says so
   in `lease.json` and in `doctor`, instead of silently pretending.
   See [DECISIONS.md](DECISIONS.md) (ADR-14).
4. **No daemon, no root required to run.** lease is a single static
   binary. It uses `bwrap` (user namespaces) and, when available,
   `systemd-run --user --scope`. It never needs a server, a container
   runtime, or setuid helpers.
5. **Self-checking.** `lease doctor` reports what the host can and cannot
   do. `lease --self-test` proves the isolation properties by actually
   trying to break them.

## Non-goals

- **Not a container runtime.** No image management, no registry, no
  orchestration, no multi-tool pipelines. One tool, one lease, one run.
- **Not a security proof.** lease is a practical sandbox, not a formally
  verified one. It has no seccomp filter (bwrap ships without one in v1),
  and a determined tool can still make any syscall the kernel allows from
  inside its namespaces. Do not run actively malicious code that is
  trying to escape a kernel-level sandbox; use a VM for that.
- **Not a scheduler.** `gc` removes finished leases; it does not run
  tools on a timer.
- **Not cross-platform.** Linux only. macOS has no equivalent of
  `bwrap`/`systemd-run`; Windows is out of scope.

## Threat model

| Attacker capability | lease's answer |
|---|---|
| Tool reads host files (home, SSH keys, `/etc`) | Root filesystem is read-only and minimal; host home is never mounted; `/etc` is a fresh tmpfs with only a few files. Verified by `--self-test` (`read-host-ssh-fails`, `write-etc-fails`). |
| Tool exfiltrates data over the network | `--unshare-net`: the child has no network interfaces at all. `localhost` mode brings up loopback only. Verified by `network-none-fails`. |
| Tool runs forever / fork-bombs | `--ttl` (lease-enforced + `RuntimeMaxSec` backup); `TasksMax=64` on the systemd path. Verified by `ttl-kills`. |
| Tool fills the disk with output | stdout/stderr are capped at 10 MB each and the cap is recorded (`stdout_truncated` in `lease.json`). |
| Tool escapes via a setuid binary | Inside the user namespace, files owned by host root appear owned by the overflow uid, so setuid to host root is impossible. (This is why dropping `NoNewPrivileges` from the scope properties is acceptable — see ADR-05.) |
| Tool reads another lease's files | Each lease has its own `<root>/<id>` with mode 0700; leases share no mounts. |
| Tool survives after the run | The launcher kills the whole process group after the run; `gc` removes the lease directory. |

What lease does **not** defend against: kernel exploits from inside a user
namespace, side channels (timing, CPU), and a tool that is given a secret
as an input (inputs are readable by design — only pass inputs the tool is
allowed to see).

## Contracts

### Commands

`run`, `gc`, `status`, `doctor`, `version`, `--self-test`. See
`plugin.toml` and the README for flags.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | the tool ran and exited 0 |
| 1 | the tool failed (non-zero exit, or OOM-killed: `CHILD_OOM`) |
| 2 | the user (or the request envelope) was wrong: bad flag, missing tool/input, input too large |
| 3 | lease itself could not run: `bwrap`/`systemd-run` missing, cannot start the sandbox, timeout (tool killed by TTL), output error |

### JSON shapes

- Success: `{"ok":true,"lease":...,"exit_code":0,...}` (see
  `schema/response.json`).
- Failure: `{"ok":false,"error":{"code":...,"blocked":...,"fix":...,"if_that_fails":...}}`
  (see `schema/error.json`). Every error code in the enum is produced by a
  named failure path in the code; no failure is reported as bare text.
- Telemetry: one JSON line per run on stderr (see `schema/telemetry.json`).
- Request envelope: `{"tool":...,"inputs":...,"out":...,"root":...,"ttl":...,"memory":...,"cpus":...,"network":...,"env":...,"argv":...}`
  (see `schema/request.json`).

### The on-disk record

`<root>/<id>/lease.json` is the source of truth for a run. It records the
inputs, the limits, whether systemd was used and why not, whether the
limits are actually enforced (`limits_enforced` + `limits_reason`), the
peak memory and how it was measured (`peak_memory_kb` +
`peak_source`), whether the child was OOM-killed, and whether the logs
were truncated. A harness should read `lease.json`, not guess from the
stdout result alone.
