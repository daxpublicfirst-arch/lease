package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunOptions carries everything `lease run` needs.
type RunOptions struct {
	Tool    string
	Inputs  []string
	Out     string
	Root    string
	TTL     int
	Memory  string
	CPUs    int
	Network string
	Env     []string
	Argv    []string
	JSON    bool
}

// RunResult is the final JSON object for a run.
type RunResult struct {
	OK         bool      `json:"ok"`
	Lease      string    `json:"lease"`
	ExitCode   int       `json:"exit_code"`
	Signal     string    `json:"signal"`
	Out        string    `json:"out"`
	Stdout     string    `json:"stdout"`
	Stderr     string    `json:"stderr"`
	DurationMs int64     `json:"duration_ms"`
	TimedOut   bool      `json:"timed_out"`
	Error      *ErrorObj `json:"error,omitempty"`
}

// buildBwrapArgs builds the bwrap command line for one run.
//
// Corrections to the original spec, verified against this host (Ubuntu 24.04,
// systemd 255) and documented in README.md:
//   - /bin, /lib and /lib64 are symlinks to /usr/bin, /usr/lib and
//     /usr/lib64, so binding them separately is wrong. We bind /usr once
//     and recreate the symlinks inside the sandbox.
//   - The spec's bwrap list never binds --tool, so the child could not exec
//     it at all. We bind the tool read-only at /tool and exec that.
//   - /etc is created with --dir (writable), so a child could create
//     /etc/lease-test. We --remount-ro /etc and / after the binds, while
//     /out, /home, /tmp and /run stay writable because they are mounts.
func buildBwrapArgs(o RunOptions, leaseDir, toolAbs string, inputsAbs []string, id string) []string {
	leaseOut := filepath.Join(leaseDir, "out")
	leaseHome := filepath.Join(leaseDir, "home")
	chdir := "/out"
	if len(inputsAbs) > 0 {
		if st, err := os.Stat(inputsAbs[0]); err == nil && st.IsDir() {
			chdir = "/input/0"
		}
	}
	args := []string{
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--dir", "/etc",
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind-try", "/etc/hosts", "/etc/hosts",
		"--dir", "/input",
	}
	for i, in := range inputsAbs {
		args = append(args, "--ro-bind", in, fmt.Sprintf("/input/%d", i))
	}
	args = append(args,
		"--bind", leaseOut, "/out",
		"--bind", leaseHome, "/home",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid",
		"--unshare-net",
		"--unshare-ipc",
		"--unshare-uts",
		"--die-with-parent",
		"--new-session",
		"--remount-ro", "/etc",
		"--remount-ro", "/",
		"--clearenv",
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
		"--setenv", "LEASE_ID", id,
		"--setenv", "LEASE_OUT", "/out",
	)
	for i := range inputsAbs {
		args = append(args, "--setenv", fmt.Sprintf("LEASE_INPUT_%d", i), fmt.Sprintf("/input/%d", i))
	}
	for _, e := range o.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k != "" {
			args = append(args, "--setenv", k, v)
		}
	}
	args = append(args, "--chdir", chdir)
	args = append(args, "--ro-bind", toolAbs, "/tool")
	if o.Network == "localhost" {
		// Loopback-only: the net namespace is unshared, so bring lo up
		// with the ip binary that /usr provides, then exec the tool.
		args = append(args, "/bin/sh", "-c", "ip link set lo up 2>/dev/null || true; exec \"$@\"", "sh", "/tool")
	} else {
		args = append(args, "/tool")
	}
	args = append(args, o.Argv...)
	return args
}
