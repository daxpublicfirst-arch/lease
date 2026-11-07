# lease — Decisions

Architecture Decision Records. Each entry states the context, the
decision, the alternatives considered, and the trade-off. The newest
entries (ADR-05, ADR-06, ADR-14, ADR-15, ADR-16, ADR-17) come from the
re-test follow-up of 2026-11-08, when the systemd path was exercised for
the first time on a host with a real user bus.

## ADR-01 — Language and dependencies: Go, standard library only

**Context.** The tool must build into one static binary with no runtime
dependencies, and must be auditable by a reader without a package
ecosystem.

**Decision.** Go, standard library only. No external modules.

**Alternatives.** Python (needs a runtime on the host; slower to start;
harder to make a static binary), Rust (stronger safety story, but a
heavier toolchain and longer build for a small CLI), shell (unmaintainable
at this size).

**Trade-off.** Go's standard library has no cgroup or D-Bus client, so
systemd and cgroup interaction is done by shelling out to `systemd-run`
and `systemctl` and by reading `/sys/fs/cgroup` files directly. That is
deliberate: it keeps the binary dependency-free and the behaviour easy to
observe (`--quiet` output, exit codes), at the cost of one subprocess per
probe.

## ADR-02 — Sandbox primitive: bwrap, not Docker, not a VM

**Context.** The sandbox must start in milliseconds, need no daemon, and
run unprivileged.

**Decision.** `bubblewrap` (`bwrap`) with user namespaces.

**Alternatives.** Docker/Podman (daemon, images, seconds to start, needs
root or a socket), a full VM (seconds-minutes, heavy), `chroot` (no
namespace isolation at all), `firejail` (larger attack surface, less
standard).

**Trade-off.** bwrap is Linux-only and needs unprivileged user namespaces
enabled in the kernel. It ships no seccomp filter in v1, so a tool can
still make any syscall the kernel allows from inside its namespaces.
This is documented as a non-goal in DESIGN.md.

**Reference.** `bwrap(1)` — https://manpages.ubuntu.com/manpages/focal/man1/bwrap.1.html

## ADR-03 — Resource limits via systemd-run --user --scope

**Context.** Memory/CPU/time limits need a mechanism that survives the
tool forking children. cgroups do that; `prlimit`/`ulimit` do not (they
apply per process).

**Decision.** When a user systemd bus exists, wrap bwrap in
`systemd-run --user --scope` with `MemoryMax`, `CPUQuota`,
`RuntimeMaxSec`, `TasksMax`. When it does not, run bwrap directly and
enforce TTL in lease itself.

**Alternatives.** Create cgroups directly in `/sys/fs/cgroup` (needs
write access to cgroupfs and reimplements what systemd already does;
fragile across hosts), `prlimit` (per-process, wrong semantics for
memory: `RLIMIT_AS` counts virtual address space, which breaks Go tools
that reserve large virtual arenas).

**Trade-off.** Limits only work where the user manager has controllers
delegated (see ADR-14). The same design is used by initrunner's
bubblewrap backend, which also warns and skips limit enforcement when
`systemd-run --user` is unavailable.

**Reference.** initrunner `docs/security/bubblewrap.md` —
https://github.com/vladkesler/initrunner/blob/HEAD/docs/security/bubblewrap.md

## ADR-04 — --scope, not a transient --service

**Context.** `systemd-run` can start the tool as a transient service
(`--wait --pipe`) or put an externally-started process into a transient
scope (`--scope`). Both were tested on systemd 255.

**Decision.** `--scope`.

**Alternatives.** Transient service with `--wait --pipe`: it accepts the
full property set, including `NoNewPrivileges` and `PrivateNetwork`
(verified working), and propagates exit codes (verified: exit 7 comes
back as 7). It was rejected because it adds a unit lifecycle and journal
chatter ("Finished with result: ...") around a synchronous run, and
stdio/exit-code handling goes through the service layer instead of being
the child's own.

**Trade-off.** `--scope` only accepts resource-control properties (see
ADR-05). If a future version needs `NoNewPrivileges`/`PrivateNetwork`
from systemd itself, switching to `--service --wait --pipe` is the
documented path — the verification above shows it works.

## ADR-05 — Scope properties: only resource-control properties (2026-11-08)

**Context.** The original spec required `NoNewPrivileges=yes` and
`PrivateNetwork=yes` on the scope. In the first build these were never
exercised, because the test VM had no user bus and every run took the
fallback path. On 2026-11-08 a user bus was started in the VM
(`dbus-daemon --system` + `systemctl start user@0.service`) and the
systemd path ran for the first time — every run failed.

**Decision.** Remove `-p NoNewPrivileges=yes` and `-p PrivateNetwork=yes`
from `buildSystemdArgs`. Scopes accept only resource-control properties.

**Evidence.** Verified on systemd 255:

```
$ systemd-run --user --scope -p NoNewPrivileges=yes -- /bin/true
Unknown assignment: NoNewPrivileges=yes
$ systemd-run --user --scope -p PrivateNetwork=yes -- /bin/true
Unknown assignment: PrivateNetwork=yes
$ systemd-run --user --scope -p MemoryMax=100M -p CPUQuota=100% \
    -p RuntimeMaxSec=60s -p TasksMax=64 -- /bin/true
Running as unit: run-....scope
```

A scope does not exec anything — `systemd-run` starts the process and
only puts it in the scope — so execution properties (`systemd.exec`)
cannot apply to it.

**Why the isolation is not lost.** The two properties were meant to
provide network isolation and setuid protection. Both already come from
bwrap: `--unshare-net` removes all network interfaces (loopback is
brought up explicitly in `localhost` mode), and inside the user namespace
a setuid binary owned by host root appears owned by the overflow uid, so
it cannot escalate to host root. The scope properties were redundant.

**Trade-off.** If a host runs bwrap without a user namespace (setuid
bwrap), the setuid argument weakens; lease always uses `--unshare-user`,
so that case does not arise. A unit test
(`TestSystemdArgsUseRuntimeMaxSec`) now asserts the forbidden properties
are absent, so this bug cannot regress silently.

## ADR-06 — --quiet on systemd-run (2026-11-08)

**Context.** `systemd-run --scope` prints `Running as unit: ...` to
stderr. lease wires the outer command's stderr to the child's stderr
log, so that line polluted every systemd-path run's stderr log.

**Decision.** Pass `--quiet` to `systemd-run` (probe and run). Verified:
with `--quiet`, both stdout and stderr logs contain exactly what the
child printed.

**Alternative.** Filter the line out of the log afterwards — fragile,
locale-dependent, and it would hide a real error if systemd ever printed
something else.

## ADR-07 — Bind /usr once; recreate /bin, /lib, /lib64 as symlinks

**Context.** On Ubuntu, `/bin`, `/lib`, `/lib64` are symlinks to `/usr/*`.
Binding them separately fails or creates duplicate mounts.

**Decision.** `--ro-bind /usr /usr`, then `--symlink usr/bin /bin`,
`--symlink usr/lib /lib`, `--symlink usr/lib64 /lib64`.

**Alternative.** Bind each real directory — breaks when the distro
layout changes, and duplicates the (large) `/usr` tree.

## ADR-08 — /etc as a tmpfs with minimal files, then remounted read-only

**Context.** The child needs `ld.so.cache` (dynamic linker), `ssl`
certificates, `resolv.conf`/`hosts` (loopback mode). A `--dir /etc`
cannot be remounted read-only afterwards.

**Decision.** `--tmpfs /etc`, bind the four files in read-only, then
`--remount-ro /etc` and `--remount-ro /` last.

**Alternative.** Bind the host `/etc` read-only — leaks host hostnames,
users, and service configuration into the sandbox. Rejected.

## ADR-09 — /proc: try a fresh mount, fall back to an empty directory

**Context.** Mounting a fresh `/proc` is forbidden on some hosts (this
cloud VM included — even `unshare --user --map-root-user --pid
--mount-proc` fails with EPERM).

**Decision.** Try `--proc /proc`; if the run fails fast and stderr
contains `Can't mount proc`, retry once with `--dir /proc` (empty) and
record `proc_fallback:true` + reason in `lease.json`. `doctor` reports
the same as a warning, not a failure.

**Trade-off.** An empty `/proc` means tools that read `/proc/self/...`
see nothing. That is a loss of introspection, not of isolation — and it
is recorded, so a harness can tell the difference.

## ADR-10 — Loopback needs a user namespace with uid 0

**Context.** `ip link set lo up` inside the sandbox failed with
"loopback: Failed RTM_NEWADDR: No child processes" until the sandbox ran
as uid 0 inside the user namespace (the container lacks `CAP_NET_ADMIN`
on the host).

**Decision.** Always `--unshare-user --uid 0 --gid 0`, and run the
loopback setup through the same wrapper as the tool.

**Alternative.** Skip loopback in `localhost` mode — would break tools
that bind a local server (the main use of the mode). Rejected.

## ADR-11 — Bind order: --ro-bind /tool before --remount-ro /

**Context.** bwrap applies filesystem options in argument order. Binding
the tool after the root is remounted read-only fails with "Can't create
file at /tool: Read-only file system".

**Decision.** Emit the `--ro-bind <tool> /tool` (and input binds) before
the final `--remount-ro /`.

## ADR-12 — Non-executable tools run via /bin/sh

**Context.** Files pushed through the GitHub API arrive as 0644, so a
script tool is not executable. Executing it directly fails with EACCES.

**Decision.** If the tool file is not executable, run it as
`/bin/sh /tool <argv>` instead of `/tool <argv>`.

**Trade-off.** A binary tool that lost its executable bit would be run
as a shell script and fail confusingly; the error message in that case
comes from `sh`, not from lease. Acceptable: the common case is scripts.

## ADR-13 — Fallback when there is no user bus

**Context.** Cron jobs, CI runners, and containers often have no user
systemd bus (`systemctl --user`: "No medium found").

**Decision.** Probe `systemd-run --user --scope` with a trivial command
(5 s timeout). On failure, run bwrap directly, enforce TTL in lease,
and record `systemd:false` + reason in `lease.json`. `doctor` reports
it as a warning, not a failure.

**Alternative.** Fail hard when there is no bus — would make lease
unusable in exactly the environments (CI, containers) where it is most
useful. Rejected.

## ADR-14 — Honest limits: report when MemoryMax/CPUQuota cannot be enforced (2026-11-08)

**Context.** The first re-test claimed "MemoryMax/CPUQuota are set" but
never verified they did anything. On 2026-11-08, with a user bus
running, a 200 MB allocation succeeded under `MemoryMax=50M`, and
`systemctl --user show <scope> -p MemoryPeak` returned `[not set]`.
The VM's cgroup view has no controllers at all: `/sys/fs/cgroup/
cgroup.controllers` is empty and there is no `memory.max` file, so the
kernel cannot enforce any cgroup limit in this container.

**Decision.** lease checks whether the user manager's cgroup actually
has the `memory.max`/`cpu.max` control files
(`limitsEnforced()` in `systemd.go`) and records the result in
`lease.json` (`limits_enforced` + `limits_reason`) and in `doctor`
(`resource-limits` check). Accepting a property is not enforcement, and
lease must not claim otherwise.

**Alternatives considered and rejected.**

- `prlimit`/`RLIMIT_AS` as a fallback enforcer: wrong semantics — it
  counts virtual address space, and Go tools reserve large virtual
  arenas, so a 2 GB `RLIMIT_AS` can kill a healthy Go tool. Rejected.
- Silently continuing to claim the limits: the original behaviour. It
  would let a harness size a job to a limit that does nothing. Rejected.
- Failing the run when limits cannot be enforced: would make lease
  unusable in containers, where isolation (filesystem/network) is still
  valuable. Rejected; the gap is surfaced, not fatal.

**Reference.** The same approach is documented by initrunner's
bubblewrap backend: "If `systemd-run --user` fails ... initrunner logs
one warning per role load and skips limit enforcement. The sandbox
itself still runs. There's no prlimit/ulimit fallback: the warning
surfaces the gap instead of silently half-enforcing."

## ADR-15 — Peak memory: cgroup memory.peak first, /proc VmHWM poller second (2026-11-08)

**Context.** The original code reported the launcher's rusage
(`wait4` maxrss). That is bwrap's parent's memory, not the tool's:
bwrap forks, the child execs the tool, and the parent only waits. The
reported number was small and unrelated to what the tool used.

**Decision.** Two layers, best available wins, and the source is
recorded (`peak_source`):

1. **cgroup** — after the run, read `memory.peak` from the scope's
   cgroup (via `systemctl --user show lease-<id>.scope -p ControlGroup`).
   This counts everything the cgroup used, including page cache (files
   the tool read/wrote), so it is the whole footprint, not just heap.
   Used only when the cgroup is visible.
2. **proc-poll** — while the run is in progress, sample the `VmHWM`
   (peak RSS) of the tool process and all its descendants every 25 ms
   (`peak.go`) and keep the maximum. Works on any host, systemd or
   fallback, because it only needs `/proc`.

**Verification.** A Python tool allocating a 200 MB bytearray and
sleeping 2 s reported `peak_memory_kb: 215332` with `peak_source:
"proc-poll"` — 200 MB plus the interpreter, as expected.

**Trade-off.** The poller can miss a tool that allocates and exits in
under one 25 ms sample interval, and it reports the highest single
process's peak, not the sum across processes. The cgroup number, when
available, has neither weakness but counts page cache, so it is larger
than the heap alone. Both limitations are documented here and in
`lease.json` via `peak_source`.

**Reference.** cgroup v2 `memory.peak` semantics and the page-cache
caveat: https://devops-daily.com/posts/how-much-memory-does-a-ci-build-need

## ADR-16 — CHILD_OOM: a distinct error for cgroup OOM kills (2026-11-08)

**Context.** When a tool exceeds `MemoryMax`, the kernel OOM-kills it
inside the cgroup. lease previously reported that as `CHILD_EXIT` with
a bare signal, indistinguishable from the tool crashing on its own.

**Decision.** After a run, read the scope cgroup's `memory.events`;
if `oom_kill > 0`, record `oom_killed:true` in `lease.json` and report
`CHILD_OOM` (exit 1) with the fix "Pass a larger --memory, or make the
tool use less memory." The code is added to `schema/error.json`.

**Trade-off.** OOM detection only works when the cgroup is visible
(the same limitation as ADR-14). On hosts without controllers, an
OOM-killed child still shows up as `CHILD_EXIT`; the gap is the same
gap, documented once, not twice.

## ADR-17 — Log write failures are OUTPUT_ERROR, not the child's fault (2026-11-08)

**Context.** During the follow-up tests, `/tmp` (a 512 MB tmpfs, 97%
full) ran out of space while a child was writing 7.5 MB of stdout.
The log write failed, the child died with SIGPIPE (exit 141), and lease
reported `CHILD_EXIT` — blaming the tool for a host disk problem.

**Decision.** `cappedWriter` records the first underlying write error.
After the run, lease reports it as `OUTPUT_ERROR` (exit 3) with the
detail "cannot write stdout log: ...", instead of the child's exit
code. The truncation cap (10 MB) is a different, recorded condition
(`stdout_truncated`/`stderr_truncated` in `lease.json`), not an error.

**Trade-off.** A child that writes exactly at the moment the disk fills
is still reported by its exit code if the log write happened to succeed;
the check only fires when a write actually failed. That is the correct
boundary: lease reports what it observed.

## ADR-18 — No tags, Releases, or CI through the GitHub App

**Context.** The Muse GitHub App cannot create tags/Releases (no such
tool) and cannot write `.github/workflows/ci.yml` (403 — it lacks the
`workflows` permission). Thomas also asked that no browser/web-UI step
be used for those.

**Decision.** Tag/Release/CI are out of scope for the agent. The CI
workflow file lives in the local checkout only (`.github/workflows/ci.yml`
is present in `~/workspace/lease-build/.github`, not in the repo).
A human with push access can add it with `git add .github && git push`.

**Trade-off.** `main` has no CI gate. The substitute is the documented
manual verification in TESTING.md: every change is tested from a clean
clone before it is reported done.

## ADR-19 — Self-test proves isolation by trying to break it

**Context.** Unit tests check argument strings; they cannot prove the
sandbox actually isolates anything.

**Decision.** `--self-test` runs 6 real sandboxed attacks against the
host: write to `/etc`, read the host SSH key, connect out on the
network, outlive the TTL, survive `gc`, and check the environment
variables. Each must fail (or be killed) in the expected way. The test
scripts are written so a failed redirection or a failed `exec` still
exits non-zero (`|| exit 3`), because `sh` does not exit on a failed
redirection by itself.

**Trade-off.** Self-test takes ~3 s and needs `bwrap` and the loopback
setup; it is a smoke test, not a fuzzing harness.
