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

	bwrapArgs := buildBwrapArgs(o, leaseDir, toolAbs, inputsAbs, id)
	useSystemd := systemdAvailable()
	var outer []string
	if useSystemd {
		lease.Systemd = true
		outer = buildSystemdArgs(o, id, bwrapArgs)
	} else {
		lease.Systemd = false
		lease.SystemdReason = "systemd-run --user --scope could not start (no user bus in this environment); ran bwrap directly with lease-enforced TTL"
		outer = append([]string{"bwrap"}, bwrapArgs...)
	}
	_ = writeLeaseJSON(leasePath, lease)

	stdoutPath := filepath.Join(leaseDir, "stdout.log")
	stderrPath := filepath.Join(leaseDir, "stderr.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create stdout log: %v", err), "Pass a writable --root.", "Check permissions on --root.", 3)
	}
	defer stdoutFile.Close()
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create stderr log: %v", err), "Pass a writable --root.", "Check permissions on --root.", 3)
	}
	defer stderrFile.Close()
	outW := &cappedWriter{f: stdoutFile, limit: MaxLogBytes}
	errW := &cappedWriter{f: stderrFile, limit: MaxLogBytes}

	// Lease-enforced TTL: systemd RuntimeMaxSec is the backup, the lease's
	// own context is what reports timed_out deterministically.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.TTL)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, outer[0], outer[1:]...)
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Inherit a minimal environment for the outer launcher only; the child
	// environment is set by bwrap --clearenv/--setenv.
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return fail("LEASE_NOT_STARTED", fmt.Sprintf("could not start the sandbox: %v", err), "Run `lease doctor` and fix what it reports.", "Check bwrap and systemd-run are installed and user namespaces are enabled.", 3)
	}
	lease.PID = cmd.Process.Pid
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		lease.PGID = pgid
	} else {
		lease.PGID = cmd.Process.Pid
	}
	_ = writeLeaseJSON(leasePath, lease)

	waitErr := cmd.Wait()
	duration := time.Since(start)
	timedOut := ctx.Err() == context.DeadlineExceeded
	if timedOut {
		KillGroup(lease.PGID)
	}
	// Make sure nothing in the group survives.
	if waitErr == nil && !timedOut {
		// Child exited on its own; still sweep the group.
		KillGroup(lease.PGID)
	}

	exitCode := 0
	signal := ""
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					signal = ws.Signal().String()
					exitCode = -1
				} else {
					exitCode = ws.ExitStatus()
				}
			} else {
				exitCode = 1
			}
		} else {
			exitCode = 1
		}
	}
	// systemd RuntimeMaxSec can kill the scope at the TTL even if our
	// context fired a moment later; treat "ran for ~TTL and died by
	// signal" as a timeout.
	if !timedOut && signal != "" && duration >= time.Duration(o.TTL)*time.Second-time.Second {
		timedOut = true
	}
	// Peak memory: rusage of the launcher is only meaningful on the direct
	// bwrap path; report it when available, 0 otherwise.
	var peakKB int64
	if ps := cmd.ProcessState; ps != nil {
		if ru, ok := ps.Sys().(*syscall.Rusage); ok {
			peakKB = ru.Maxrss // KB on Linux
		}
	}
	lease.Ended = time.Now()
	lease.TimedOut = timedOut
	lease.Signal = signal
	ec := exitCode
	lease.ExitCode = &ec
	lease.PeakMemoryKB = peakKB
	_ = writeLeaseJSON(leasePath, lease)

	// Copy the sandbox /out to the host --out.
	if err := os.MkdirAll(outAbs, 0o755); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot create --out %s: %v", outAbs, err), "Pass an --out under a writable directory.", "Check permissions on the parent directory.", 3)
	}
	if err := copyDir(filepath.Join(leaseDir, "out"), outAbs); err != nil {
		return fail("OUTPUT_ERROR", fmt.Sprintf("cannot copy sandbox output to --out: %v", err), "Check that --out is writable.", "Re-run with a different --out.", 3)
	}

	outputBytes := outW.written + errW.written
	res := RunResult{
		Lease: id, ExitCode: exitCode, Signal: signal, Out: outAbs,
		Stdout: stdoutPath, Stderr: stderrPath,
		DurationMs: duration.Milliseconds(), TimedOut: timedOut,
	}
	if timedOut {
		res.OK = false
		res.Error = &ErrorObj{Code: "TIMED_OUT", Blocked: fmt.Sprintf("child ran longer than --ttl %ds and was killed", o.TTL), Fix: "Pass a larger --ttl, or make the tool faster.", IfThatFails: "Run the tool outside lease to see why it is slow."}
		emitTelemetry(false, inputBytes, outputBytes, duration)
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
	if exitCode != 0 {
		res.OK = false
		res.Error = &ErrorObj{Code: "CHILD_EXIT", Blocked: fmt.Sprintf("child exited with code %d (signal %q)", exitCode, signal), Fix: "Run the tool outside lease with the same inputs to reproduce the failure.", IfThatFails: "Check the stderr log for the tool's own error message."}
		emitTelemetry(false, inputBytes, outputBytes, duration)
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
	emitTelemetry(true, inputBytes, outputBytes, duration)
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
