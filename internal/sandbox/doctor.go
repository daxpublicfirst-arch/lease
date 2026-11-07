package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Check is one doctor check.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// DoctorResult is the `lease doctor` result.
type DoctorResult struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Doctor checks bwrap, systemd-run, user namespaces and a writable root.
func Doctor(root string) DoctorResult {
	checks := []Check{}

	if p, err := exec.LookPath("bwrap"); err == nil {
		checks = append(checks, Check{Name: "bwrap", OK: true, Detail: "found at " + p})
	} else {
		checks = append(checks, Check{Name: "bwrap", OK: false, Detail: "not found in PATH; install bubblewrap (apt-get install bubblewrap)"})
	}
	if p, err := exec.LookPath("systemd-run"); err == nil {
		checks = append(checks, Check{Name: "systemd-run", OK: true, Detail: "found at " + p})
		if systemdAvailable() {
			checks = append(checks, Check{Name: "systemd-user", OK: true, Detail: "systemd-run --user --scope can start a command"})
		} else {
			// Not fatal: Run falls back to direct bwrap with lease TTL.
			checks = append(checks, Check{Name: "systemd-user", OK: true, Detail: "warning: no user systemd bus in this environment; lease will run bwrap directly with lease-enforced TTL"})
		}
	} else {
		checks = append(checks, Check{Name: "systemd-run", OK: false, Detail: "not found in PATH; install systemd or run on a systemd Linux host"})
	}

	nsOK, nsDetail := checkUserNS()
	checks = append(checks, Check{Name: "user-namespaces", OK: nsOK, Detail: nsDetail})

	if root == "" {
		root = "/tmp/arabica-lease"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		checks = append(checks, Check{Name: "writable-root", OK: false, Detail: fmt.Sprintf("cannot create %s: %v", root, err)})
	} else if d, err := os.MkdirTemp(root, ".doctor-"); err != nil {
		checks = append(checks, Check{Name: "writable-root", OK: false, Detail: fmt.Sprintf("cannot write under %s: %v", root, err)})
	} else {
		_ = os.RemoveAll(d)
		checks = append(checks, Check{Name: "writable-root", OK: true, Detail: "writable: " + root})
	}

	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	return DoctorResult{OK: ok, Checks: checks}
}

func checkUserNS() (bool, string) {
	if b, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n == 0 {
			return false, "user namespaces are disabled (max_user_namespaces=0)"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--", "/bin/true")
	if err := cmd.Run(); err != nil {
		return false, fmt.Sprintf("unshare --user failed: %v; enable unprivileged user namespaces", err)
	}
	return true, "unprivileged user namespaces work"
}

// PrintDoctor prints the doctor result, human-readable or JSON.
func PrintDoctor(r DoctorResult, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
		return
	}
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("%s %-18s %s\n", mark, c.Name, c.Detail)
	}
	if !r.OK {
		fmt.Println("doctor: some checks failed (see above)")
	}
}

// DoctorRoot is the default root used by doctor when none is given.
func DoctorRoot() string {
	if v := os.Getenv("LEASE_ROOT"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "arabica-lease")
}
