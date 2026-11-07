package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// execOutcome is the result of one execute() attempt.
type execOutcome struct {
	exitCode                         int
	signal                           string
	timedOut                         bool
	peakKB                           int64
	peakSource                       string
	oomKills                         int
	duration                         time.Duration
	outputBytes                      int64
	stdoutTruncated, stderrTruncated bool
	startErr                         error
}

// Run executes one sandboxed run and returns the result plus the process
// exit code lease itself should exit with (0, 1, 2 or 3).
func Run(o RunOptions) (RunResult, int) {
	start := time.Now()
	emitTelemetry := func(ok bool, inputBytes, outputBytes int64, dur time.Duration) {
		line, _ := json.Marshal(map[string]any{
			"tool": "lease", "version": Version, "command": "run",
			"duration_ms": dur.Milliseconds(), "input_bytes": inputBytes,
			"output_bytes": outputBytes, "ok": ok,
		})
		fmt.Fprintln(os.Stderr, string(line))
	}
	fail := func(code, blocked, fix, ifThatFails string, exit int) (RunResult, int) {
		f := NewError(code, blocked, fix, ifThatFails)
		PrintFailure(f)
		emitTelemetry(false, 0, 0, time.Since(start))
		return RunResult{OK: false, Error: &f.Error, DurationMs: time.Since(start).Milliseconds()}, exit
	}

	if o.TTL <= 0 {
		return fail("USAGE", "--ttl must be a positive number of seconds", "Pass --ttl 60 (or another positive value).", "Check `lease run --help`.", 2)
	}
	if o.CPUs < 1 {
		return fail("USAGE", "--cpus must be at least 1", "Pass --cpus 2 (or another value >= 1).", "Check `lease run --help`.", 2)
	}
	if o.Network != "none" && o.Network != "localhost" {
		return fail("USAGE", "--network must be none or localhost", "Pass --network none (or localhost for loopback only).", "Check `lease run --help`.", 2)
	}
	if o.Tool == "" {
		return fail("USAGE", "--tool is required", "Pass --tool <path> to the tool binary or script.", "Check `lease run --help`.", 2)
	}
	if o.Out == "" {
		return fail("USAGE", "--out is required", "Pass --out <dir> for the host output directory.", "Check `lease run --help`.", 2)
	}
	toolAbs, err := absOrDie(o.Tool)
	if err != nil {
		return fail("USAGE", "bad --tool path", "Pass an existing tool path.", "Check `lease run --help`.", 2)
	}
	if st, err := os.Stat(toolAbs); err != nil || st.IsDir() {
		return fail("TOOL_NOT_FOUND", fmt.Sprintf("tool not found: %s", toolAbs), "Pass --tool <path> to an existing executable file.", "Check the path and try again.", 2)
	}
	rootAbs, err := absOrDie(o.Root)
	if err != nil {
		return fail("USAGE", "bad --root path", "Pass an existing or creatable --root directory.", "Check `lease run --help`.", 2)
	}
	outAbs, err := absOrDie(o.Out)
	if err != nil {
		return fail("USAGE", "bad --out path", "Pass --out <dir>.", "Check `lease run --help`.", 2)
	}
	inputsAbs := make([]string, 0, len(o.Inputs))
	var inputBytes int64
	for _, in := range o.Inputs {
		abs, err := absOrDie(in)
		if err != nil {
			return fail("USAGE", "bad --input path", "Pass existing --input paths.", "Check `lease run --help`.", 2)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return fail("INPUT_NOT_FOUND", fmt.Sprintf("input not found: %s", abs), "Pass --input <path> for files that exist.", "Check the path and try again.", 2)
		}
		if !st.IsDir() {
			inputBytes += st.Size()
		}
		inputsAbs = append(inputsAbs, abs)
	}
	if inputBytes > MaxInputBytes {
		return fail("INPUT_TOO_LARGE", fmt.Sprintf("inputs total %d bytes, over the 200 MB limit", inputBytes), "Pass smaller inputs (limit is 200 MB total).", "Split the input or raise the limit in plugin.toml and rebuild.", 2)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fail("BWRAP_MISSING", "bwrap was not found in PATH", "Install bubblewrap (e.g. apt-get install bubblewrap, dnf install bubblewrap, or brew install bubblewrap).", "Install bwrap from your distribution, then run `lease doctor`.", 3)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fail("SYSTEMD_MISSING", "systemd-run was not found in PATH", "Install systemd (it ships systemd-run) or run on a Linux host with systemd.", "Run on a systemd Linux host, then run `lease doctor`.", 3)
	}
	if err := os.MkdirAll(rootAbs, 0o755); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create --root %s: %v", rootAbs, err), "Pass a --root under a writable directory.", "Check permissions on the parent directory.", 3)
	}

	id, err := NewID()
	if err != nil {
		return fail("LEASE_NOT_STARTED", fmt.Sprintf("cannot generate a lease id: %v", err), "Retry the run.", "Check the system entropy source.", 3)
	}
	leaseDir := LeaseDir(rootAbs, id)
	if err := os.MkdirAll(filepath.Join(leaseDir, "out"), 0o700); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create lease dir %s: %v", leaseDir, err), "Pass a writable --root.", "Check permissions on --root.", 3)
	}
	if err := os.MkdirAll(filepath.Join(leaseDir, "home"), 0o700); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create lease home %s: %v", leaseDir, err), "Pass a writable --root.", "Check permissions on --root.", 3)
	}
	lease := &Lease{
		ID: id, Tool: toolAbs, Inputs: inputsAbs, Out: outAbs, Root: rootAbs,
		Start: start, TTL: o.TTL, Network: o.Network,
		Limits: Limits{InputMB: 200, TimeoutS: o.TTL, Memory: o.Memory, CPUs: o.CPUs, Network: o.Network},
	}
	leasePath := filepath.Join(leaseDir, "lease.json")
	_ = writeLeaseJSON(leasePath, lease)

	useSystemd := systemdAvailable()
	lease.Systemd = useSystemd
	if !useSystemd {
		lease.SystemdReason = "systemd-run --user --scope could not start (no user bus in this environment); ran bwrap directly with lease-enforced TTL"
		lease.LimitsEnforced = false
		lease.LimitsReason = "systemd-run --user unavailable: MemoryMax/CPUQuota cannot be enforced (lease still isolates filesystem and network)"
	} else {
		// systemd accepted the properties, but that is not enforcement:
		// the kernel only enforces them when the user cgroup has the
		// memory/cpu controllers delegated. Record what is actually true.
		memOK, cpuOK, reason := limitsEnforced()
		lease.LimitsEnforced = memOK && cpuOK
		lease.LimitsReason = reason
	}
	_ = writeLeaseJSON(leasePath, lease)

	stdoutPath := filepath.Join(leaseDir, "stdout.log")
	stderrPath := filepath.Join(leaseDir, "stderr.log")

	// execute runs one attempt. It (re)creates the log files, so a retry
	// starts with clean logs.
	execute := func(useProc bool) execOutcome {
		bwrapArgs := buildBwrapArgs(o, leaseDir, toolAbs, inputsAbs, id, useProc)
		var outer []string
		if useSystemd {
			outer = buildSystemdArgs(o, id, bwrapArgs)
		} else {
			outer = append([]string{"bwrap"}, bwrapArgs...)
		}
		stdoutFile, err := os.Create(stdoutPath)
		if err != nil {
			return execOutcome{startErr: fmt.Errorf("cannot create stdout log: %w", err)}
		}
		stderrFile, err := os.Create(stderrPath)
		if err != nil {
			stdoutFile.Close()
			return execOutcome{startErr: fmt.Errorf("cannot create stderr log: %w", err)}
		}
		outW := &cappedWriter{f: stdoutFile, limit: MaxLogBytes}
		errW := &cappedWriter{f: stderrFile, limit: MaxLogBytes}

		// Lease-enforced TTL: systemd RuntimeMaxSec is the backup, the
		// lease's own context is what reports timed_out deterministically.
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.TTL)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, outer[0], outer[1:]...)
		cmd.Stdout = outW
		cmd.Stderr = errW
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Inherit the environment for the outer launcher only; the child
		// environment is set by bwrap --clearenv/--setenv.
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			stdoutFile.Close()
			stderrFile.Close()
			return execOutcome{startErr: fmt.Errorf("could not start the sandbox: %w", err)}
		}
		lease.PID = cmd.Process.Pid
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			lease.PGID = pgid
		} else {
			lease.PGID = cmd.Process.Pid
		}
		_ = writeLeaseJSON(leasePath, lease)

		// Peak memory: sample the tool process tree's VmHWM while it runs.
		// (The old code reported the launcher's rusage, which is bwrap's
		// own memory, not the tool's. See peak.go.)
		poller := newPeakPoller(cmd.Process.Pid)

		waitErr := cmd.Wait()
		polledKB := poller.stopAndRead()
		duration := time.Since(start)
		timedOut := ctx.Err() == context.DeadlineExceeded
		if timedOut {
			KillGroup(lease.PGID)
		} else if waitErr == nil {
			// Child exited on its own; still sweep the group.
			KillGroup(lease.PGID)
		}

		out := execOutcome{duration: duration, timedOut: timedOut}
		if waitErr != nil {
			if ee, ok := waitErr.(*exec.ExitError); ok {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
					if ws.Signaled() {
						out.signal = ws.Signal().String()
						out.exitCode = -1
					} else {
						out.exitCode = ws.ExitStatus()
					}
				} else {
					out.exitCode = 1
				}
			} else {
				out.exitCode = 1
			}
		}
		// systemd RuntimeMaxSec can kill the scope at the TTL even if our
		// context fired a moment later; treat "ran for ~TTL and died by
		// signal" as a timeout.
		if !out.timedOut && out.signal != "" && duration >= time.Duration(o.TTL)*time.Second-time.Second {
			out.timedOut = true
		}
		// Peak memory, best source first: the scope cgroup's memory.peak
		// when it is visible on this host, else the poller's sampled max.
		if useSystemd {
			cg := scopeCgroupPath(id)
			if cgPeak := cgroupPeakKB(cg); cgPeak > 0 {
				out.peakKB = cgPeak
				out.peakSource = "cgroup"
			}
			out.oomKills = cgroupOOMKills(cg)
		}
		if out.peakKB == 0 {
			out.peakKB = polledKB
			if polledKB > 0 {
				out.peakSource = "proc-poll"
			}
		}
		out.outputBytes = outW.written + errW.written
		out.stdoutTruncated = outW.truncated
		out.stderrTruncated = errW.truncated
		stdoutFile.Close()
		stderrFile.Close()
		return out
	}

	out := execute(true)
	if out.startErr != nil {
		return fail("LEASE_NOT_STARTED", out.startErr.Error(), "Run `lease doctor` and fix what it reports.", "Check bwrap and systemd-run are installed and user namespaces are enabled.", 3)
	}
	// Mounting a fresh proc is impossible on some hosts (this cloud VM
	// included): bwrap fails fast with "Can't mount proc". Retry once
	// with an empty /proc directory instead of a proc mount.
	if out.exitCode != 0 && !out.timedOut {
		if b, err := os.ReadFile(stderrPath); err == nil && strings.Contains(string(b), "Can't mount proc") {
			lease.ProcFallback = true
			lease.ProcReason = "mounting a fresh /proc is not permitted on this host; the sandbox uses an empty /proc instead"
			_ = writeLeaseJSON(leasePath, lease)
			out = execute(false)
			if out.startErr != nil {
				return fail("LEASE_NOT_STARTED", out.startErr.Error(), "Run `lease doctor` and fix what it reports.", "Check bwrap and systemd-run are installed and user namespaces are enabled.", 3)
			}
		}
	}

	lease.Ended = time.Now()
	lease.TimedOut = out.timedOut
	lease.Signal = out.signal
	ec := out.exitCode
	lease.ExitCode = &ec
	lease.PeakMemoryKB = out.peakKB
	lease.PeakSource = out.peakSource
	lease.OOMKilled = out.oomKills > 0
	lease.StdoutTruncated = out.stdoutTruncated
	lease.StderrTruncated = out.stderrTruncated
	_ = writeLeaseJSON(leasePath, lease)

	// Copy the sandbox /out to the host --out.
	if err := os.MkdirAll(outAbs, 0o755); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create --out %s: %v", outAbs, err), "Pass an --out under a writable directory.", "Check permissions on the parent directory.", 3)
	}
	if err := copyDir(filepath.Join(leaseDir, "out"), outAbs); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot copy sandbox output to --out: %v", err), "Check that --out is writable.", "Re-run with a different --out.", 3)
	}

	res := RunResult{
		Lease: id, ExitCode: out.exitCode, Signal: out.signal, Out: outAbs,
		Stdout: stdoutPath, Stderr: stderrPath,
		DurationMs: out.duration.Milliseconds(), TimedOut: out.timedOut,
	}
	if out.timedOut {
		res.OK = false
		res.Error = &ErrorObj{Code: "TIMED_OUT", Blocked: fmt.Sprintf("child ran longer than --ttl %ds and was killed", o.TTL), Fix: "Pass a larger --ttl, or make the tool faster.", IfThatFails: "Run the tool outside lease to see why it is slow."}
		emitTelemetry(false, inputBytes, out.outputBytes, out.duration)
		if o.JSON {
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
		} else {
			printChildStdout(stdoutPath)
			f := NewError("TIMED_OUT", res.Error.Blocked, res.Error.Fix, res.Error.IfThatFails)
			PrintFailure(f)
		}
		return res, 3
	}
	if out.oomKills > 0 {
		res.OK = false
		res.Error = &ErrorObj{Code: "CHILD_OOM", Blocked: fmt.Sprintf("child was killed by the kernel OOM killer inside its cgroup (limit --memory %s)", o.Memory), Fix: "Pass a larger --memory, or make the tool use less memory.", IfThatFails: "Run the tool outside lease and watch its memory use."}
		emitTelemetry(false, inputBytes, out.outputBytes, out.duration)
		if o.JSON {
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
		} else {
			printChildStdout(stdoutPath)
			f := NewError("CHILD_OOM", res.Error.Blocked, res.Error.Fix, res.Error.IfThatFails)
			PrintFailure(f)
		}
		return res, 1
	}
	if out.exitCode != 0 {
		res.OK = false
		res.Error = &ErrorObj{Code: "CHILD_EXIT", Blocked: fmt.Sprintf("child exited with code %d (signal %q)", out.exitCode, out.signal), Fix: "Run the tool outside lease with the same inputs to reproduce the failure.", IfThatFails: "Check the stderr log for the tool's own error message."}
		emitTelemetry(false, inputBytes, out.outputBytes, out.duration)
		if o.JSON {
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
		} else {
			printChildStdout(stdoutPath)
			f := NewError("CHILD_EXIT", res.Error.Blocked, res.Error.Fix, res.Error.IfThatFails)
			PrintFailure(f)
		}
		return res, 1
	}
	res.OK = true
	emitTelemetry(true, inputBytes, out.outputBytes, out.duration)
	if o.JSON {
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
	} else {
		printChildStdout(stdoutPath)
	}
	return res, 0
}

func printChildStdout(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Print exactly what the child printed.
	fmt.Print(string(b))
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		fmt.Println()
	}
}
