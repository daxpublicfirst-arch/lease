// Package sandbox implements one disposable run of a CLI tool under bwrap
// (inside systemd-run when a user systemd bus is available).
package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Version is the lease version. It must match plugin.toml and CHANGELOG.md.
const Version = "0.1.0"

// MaxInputBytes is the total input size limit from plugin.toml (200 MB).
const MaxInputBytes = 200 * 1024 * 1024

// MaxLogBytes caps each captured stdout/stderr log at 10 MB.
const MaxLogBytes = 10 * 1024 * 1024

// ErrorObj is the failure shape printed on any failure.
type ErrorObj struct {
	Code        string `json:"code"`
	Blocked     string `json:"blocked"`
	Fix         string `json:"fix"`
	IfThatFails string `json:"if_that_fails"`
}

// Failure is one JSON object: {"ok":false,"error":{...}}.
type Failure struct {
	OK    bool     `json:"ok"`
	Error ErrorObj `json:"error"`
}

// NewError builds a Failure with all four fields filled.
func NewError(code, blocked, fix, ifThatFails string) Failure {
	return Failure{OK: false, Error: ErrorObj{Code: code, Blocked: blocked, Fix: fix, IfThatFails: ifThatFails}}
}

// PrintFailure writes one JSON object to stdout.
func PrintFailure(f Failure) {
	b, _ := json.Marshal(f)
	fmt.Println(string(b))
}

// Limits records the resource limits applied to a run.
type Limits struct {
	InputMB  int    `json:"input_mb"`
	TimeoutS int    `json:"timeout_s"`
	Memory   string `json:"memory"`
	CPUs     int    `json:"cpus"`
	Network  string `json:"network"`
}

// Lease is the on-disk record written to <root>/<id>/lease.json.
type Lease struct {
	ID           string    `json:"id"`
	Tool         string    `json:"tool"`
	Inputs       []string  `json:"inputs"`
	Out          string    `json:"out"`
	Root         string    `json:"root"`
	PID          int       `json:"pid"`
	PGID         int       `json:"pgid"`
	Start        time.Time `json:"start"`
	Ended        time.Time `json:"ended,omitempty"`
	TTL          int       `json:"ttl"`
	Limits       Limits    `json:"limits"`
	Network      string    `json:"network"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	Signal       string    `json:"signal,omitempty"`
	TimedOut     bool      `json:"timed_out"`
	PeakMemoryKB int64     `json:"peak_memory_kb,omitempty"`
	// PeakSource is "cgroup" when the number came from the scope cgroup's
	// memory.peak, "proc-poll" when it came from sampling /proc VmHWM.
	// The cgroup number counts page cache too; the poller number is the
	// highest single process's peak RSS sampled every 25 ms.
	PeakSource string `json:"peak_source,omitempty"`
	// LimitsEnforced is true only when the kernel will really enforce
	// MemoryMax/CPUQuota for this run (systemd path + delegated cgroup
	// controllers). See limitsEnforced in systemd.go.
	LimitsEnforced  bool   `json:"limits_enforced"`
	LimitsReason    string `json:"limits_reason,omitempty"`
	OOMKilled       bool   `json:"oom_killed,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	Systemd         bool   `json:"systemd"`
	SystemdReason   string `json:"systemd_reason,omitempty"`
	ProcFallback    bool   `json:"proc_fallback,omitempty"`
	ProcReason      string `json:"proc_reason,omitempty"`
}

// LeaseDir returns <root>/<id>.
func LeaseDir(root, id string) string { return filepath.Join(root, id) }

func writeLeaseJSON(path string, l *Lease) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ReadLease reads a lease.json file.
func ReadLease(path string) (*Lease, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// NewID returns a random 32-char hex lease id.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Alive reports whether pid is still running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	// A zombie still answers kill(pid, 0); check /proc state on Linux.
	if st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// State is the field after the closing paren of comm.
		for i := len(st) - 1; i >= 0; i-- {
			if st[i] == ')' && i+2 < len(st) {
				return st[i+2] != 'Z'
			}
		}
	}
	return true
}

// KillGroup kills a whole process group (negative pgid), best effort.
func KillGroup(pgid int) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// cappedWriter writes to a file but stops after limit bytes and records
// whether anything was truncated.
type cappedWriter struct {
	f         *os.File
	limit     int64
	written   int64
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.written >= w.limit {
		w.truncated = true
		return len(p), nil
	}
	room := w.limit - w.written
	if int64(len(p)) > room {
		p = p[:room]
		w.truncated = true
	}
	n, err := w.f.Write(p)
	w.written += int64(n)
	return len(p), err
}

// copyDir copies a directory tree (files only, no symlinks followed).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// absOrDie resolves a path to absolute form.
func absOrDie(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	return filepath.Abs(p)
}
