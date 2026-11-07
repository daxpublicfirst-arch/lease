package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// systemdAvailable probes whether `systemd-run --user --scope` can start a
// trivial command. In containers/VMs without a user bus (no
// XDG_RUNTIME_DIR, no user@.service) this fails fast, and Run falls back to
// running bwrap directly. The probe is cheap and never touches the lease.
func systemdAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run", "--user", "--scope",
		"--quiet", "-p", "RuntimeMaxSec=10s", "--", "/bin/true")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// buildSystemdArgs wraps a bwrap invocation in systemd-run --user --scope.
// Note the property is RuntimeMaxSec, not RuntimeMax.
//
// Only resource-control properties are valid on a scope. Verified against
// systemd 255: `-p NoNewPrivileges=yes` and `-p PrivateNetwork=yes` are both
// rejected with "Unknown assignment", so a scope invocation that includes
// them fails every run on any host that actually has a user bus. They are
// execution (systemd.exec) properties, not scope properties, and a scope
// does not exec anything — systemd-run starts the process itself and only
// puts it in the scope. The isolation those two were meant to provide
// already comes from bwrap: --unshare-net gives the child no network (or
// loopback only), and inside the user namespace a setuid binary owned by
// host root appears owned by the overflow uid, so it cannot escalate to
// host root. See docs/DECISIONS.md. The alternative — a transient service
// with --wait --pipe — does accept the full property set, but adds a unit
// lifecycle and journal chatter for no isolation gain; --scope stays.
func buildSystemdArgs(o RunOptions, id string, bwrapArgs []string) []string {
	args := []string{
		"systemd-run", "--user", "--scope",
		"--quiet", // keep "Running as unit: ..." out of the child's stderr log
		"--unit=lease-" + id,
		"-p", "MemoryMax=" + o.Memory,
		"-p", fmt.Sprintf("CPUQuota=%d%%", o.CPUs*100),
		"-p", fmt.Sprintf("RuntimeMaxSec=%ds", o.TTL),
		"-p", "TasksMax=64",
	}
	args = append(args, "--", "bwrap")
	return append(args, bwrapArgs...)
}

// scopeCgroupPath returns the cgroup path of a lease scope, e.g.
// "/user.slice/user-1000.slice/user@1000.service/app.slice/lease-<id>.scope",
// or "" when it cannot be determined (no bus, unit already gone).
func scopeCgroupPath(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show",
		"lease-"+id+".scope", "-p", "ControlGroup", "--value")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readCgroupFile reads one file under /sys/fs/cgroup/<cgroupPath>.
// It returns "" on any error.
func readCgroupFile(cgroupPath, name string) string {
	if cgroupPath == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cgroupPath, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// cgroupPeakKB reads memory.peak (bytes) of a scope cgroup and converts to
// KiB. It returns 0 when the cgroup is not visible — which is the case in
// containers whose cgroup view has no delegated controllers.
func cgroupPeakKB(cgroupPath string) int64 {
	v := readCgroupFile(cgroupPath, "memory.peak")
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n / 1024
	}
	return 0
}

// cgroupOOMKills reads the oom_kill count from a scope cgroup's
// memory.events. It returns 0 when the cgroup is not visible.
func cgroupOOMKills(cgroupPath string) int {
	v := readCgroupFile(cgroupPath, "memory.events")
	for _, line := range strings.Split(v, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "oom_kill" {
			if n, err := strconv.Atoi(f[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

// limitsEnforced reports whether the kernel will actually enforce the
// systemd resource limits on this host, and why not when it will not.
// Accepting a property is not enforcement: systemd-run happily accepts
// MemoryMax on a host whose cgroup view has no memory controller, and the
// limit then silently does nothing (verified: a 200 MB allocation
// succeeded under MemoryMax=50M in this VM). The check is therefore
// whether the user manager's cgroup has the memory/cpu control files.
// It mirrors the approach of initrunner's bubblewrap backend, which warns
// and skips limit enforcement instead of silently half-enforcing.
func limitsEnforced() (memoryOK, cpuOK bool, reason string) {
	cg := ""
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show",
		"-p", "ControlGroup", "--value")
	if out, err := cmd.Output(); err == nil {
		cg = strings.TrimSpace(string(out))
	}
	if cg == "" {
		return false, false, "no user systemd bus: resource limits cannot be enforced (lease still isolates filesystem and network)"
	}
	memFile := filepath.Join("/sys/fs/cgroup", cg, "memory.max")
	cpuFile := filepath.Join("/sys/fs/cgroup", cg, "cpu.max")
	if _, err := os.Stat(memFile); err != nil {
		memoryOK = false
	} else {
		memoryOK = true
	}
	if _, err := os.Stat(cpuFile); err != nil {
		cpuOK = false
	} else {
		cpuOK = true
	}
	switch {
	case memoryOK && cpuOK:
		return true, true, ""
	case !memoryOK && !cpuOK:
		return false, false, "user cgroup has no memory or cpu controller delegated: MemoryMax/CPUQuota will be accepted but not enforced on this host"
	case !memoryOK:
		return false, true, "user cgroup has no memory controller delegated: MemoryMax will be accepted but not enforced on this host"
	default:
		return true, false, "user cgroup has no cpu controller delegated: CPUQuota will be accepted but not enforced on this host"
	}
}
