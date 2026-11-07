package sandbox

import (
	"context"
	"fmt"
	"os/exec"
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
		"-p", "RuntimeMaxSec=10s", "--", "/bin/true")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// buildSystemdArgs wraps a bwrap invocation in systemd-run --user --scope.
// Note the property is RuntimeMaxSec, not RuntimeMax.
func buildSystemdArgs(o RunOptions, id string, bwrapArgs []string) []string {
	args := []string{
		"systemd-run", "--user", "--scope",
		"--unit=lease-" + id,
		"-p", "MemoryMax=" + o.Memory,
		"-p", fmt.Sprintf("CPUQuota=%d%%", o.CPUs*100),
		"-p", fmt.Sprintf("RuntimeMaxSec=%ds", o.TTL),
		"-p", "TasksMax=64",
		"-p", "NoNewPrivileges=yes",
	}
	if o.Network == "none" {
		args = append(args, "-p", "PrivateNetwork=yes")
	}
	args = append(args, "--", "bwrap")
	return append(args, bwrapArgs...)
}
