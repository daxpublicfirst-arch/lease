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
//     /etc/lease-test. We mount a tmpfs on /etc, bind the minimal files
//     into it, and --remount-ro /etc (a plain --dir cannot be remounted).
//   - --unshare-user --uid 0 --gid 0 (bwrap's portable rootless mode):
//     without it, loopback setup fails in this VM ("Failed RTM_NEWADDR:
//     No child processes") because the container lacks CAP_NET_ADMIN.
//   - useProc selects --proc /proc (spec) or --dir /proc (empty). Mounting
//     a fresh proc is impossible in this VM (even plain
//     `unshare --user --map-root-user --pid --mount-proc` fails with
//     EPERM), so Run retries with an empty /proc when the first attempt
//     fails that way. The child sees no host processes either way.
func buildBwrapArgs(o RunOptions, leaseDir, toolAbs string, inputsAbs []string, id string, useProc bool) []string {
	leaseOut := filepath.Join(leaseDir, "out")
	leaseHome := filepath.Join(leaseDir, "home")
	chdir := "/out"
	if len(inputsAbs) > 0 {
		if st, err := os.Stat(inputsAbs[0]); err == nil && st.IsDir() {
			chdir = "/input/0"
		}
	}
	args := []string{
		"--unshare-user", "--uid", "0", "--gid", "0",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--tmpfs", "/etc",
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind-try", "/etc/hosts", "/etc/hosts",
		"--dir", "/input",
	}
	for i, in := range inputsAbs {
		args = append(args, "--ro-bind", in, fmt.Sprintf("/input/%d", i))
	}
	// The tool bind must come BEFORE --remount-ro / below: once / is
	// remounted read-only, bwrap cannot create the /tool mount point.
	args = append(args, "--ro-bind", toolAbs, "/tool")
	args = append(args,
		"--bind", leaseOut, "/out",
		"--bind", leaseHome, "/home",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
	)
	if useProc {
		args = append(args, "--proc", "/proc")
	} else {
		args = append(args, "--dir", "/proc")
	}
	args = append(args,
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
	// Exec form: an executable tool runs directly; a non-executable one
	// (e.g. a shell script checked in without the exec bit, which the
	// GitHub API stores as 0644) runs under /bin/sh instead of failing
	// with EACCES.
	execArgs := []string{"/tool"}
	if st, err := os.Stat(toolAbs); err == nil && st.Mode()&0o111 == 0 {
		execArgs = []string{"/bin/sh", "/tool"}
	}
	if o.Network == "localhost" {
		// Loopback-only: the net namespace is unshared, so bring lo up
		// with the ip binary that /usr provides, then exec the tool.
		args = append(args, "/bin/sh", "-c", "ip link set lo up 2>/dev/null || true; exec \"$@\"", "sh")
		args = append(args, execArgs...)
	} else {
		args = append(args, execArgs...)
	}
	args = append(args, o.Argv...)
	return args
}
