package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewIDFormat(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 {
		t.Fatalf("id %q should be 32 hex chars", id)
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("id %q is not hex", id)
		}
	}
}

func TestBwrapArgsBindsToolAndInputs(t *testing.T) {
	o := RunOptions{Tool: "/tmp/tool", Inputs: []string{"/tmp/in.log"}, Out: "/tmp/out", Root: "/tmp/root", TTL: 60, Memory: "2G", CPUs: 2, Network: "none", Argv: []string{"a", "b"}}
	args := buildBwrapArgs(o, "/tmp/root/abc", "/tmp/tool", []string{"/tmp/in.log"}, "abc", true)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--ro-bind /usr /usr",
		"--symlink usr/bin /bin",
		"--ro-bind /tmp/in.log /input/0",
		"--bind /tmp/root/abc/out /out",
		"--ro-bind /tmp/tool /tool",
		"--remount-ro /etc",
		"--setenv LEASE_OUT /out",
		"--setenv LEASE_INPUT_0 /input/0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap args missing %q in %q", want, joined)
		}
	}
	if !strings.Contains(joined, "--unshare-user") || !strings.Contains(joined, "--tmpfs /etc") {
		t.Errorf("bwrap args must use --unshare-user and a tmpfs /etc: %q", joined)
	}
	if strings.Contains(joined, "--ro-bind /bin /bin") || strings.Contains(joined, "--ro-bind /lib /lib") {
		t.Errorf("must not bind symlinked /bin or /lib separately: %q", joined)
	}
	if !strings.HasSuffix(joined, "/tool a b") {
		t.Errorf("tool + argv should come last: %q", joined)
	}
}

func TestBwrapArgsLocalhostBringsLoopbackUp(t *testing.T) {
	o := RunOptions{Network: "localhost", TTL: 60, Memory: "2G", CPUs: 2}
	args := buildBwrapArgs(o, "/tmp/root/abc", "/tmp/tool", nil, "abc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ip link set lo up") {
		t.Errorf("localhost mode must bring lo up: %q", joined)
	}
}

func TestSystemdArgsUseRuntimeMaxSec(t *testing.T) {
	o := RunOptions{TTL: 60, Memory: "2G", CPUs: 2, Network: "none"}
	args := buildSystemdArgs(o, "abc", []string{"bwrap", "--ro-bind", "/usr", "/usr"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"RuntimeMaxSec=60s", "MemoryMax=2G", "CPUQuota=200%", "TasksMax=64", "--quiet", "systemd-run --user --scope"} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemd args missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "RuntimeMax=60") {
		t.Errorf("must use RuntimeMaxSec, not RuntimeMax: %q", joined)
	}
	// Scopes only accept resource-control properties. NoNewPrivileges and
	// PrivateNetwork are systemd.exec properties: systemd rejects them
	// with "Unknown assignment", so including them broke every run on
	// any host that actually has a user bus.
	for _, forbidden := range []string{"NoNewPrivileges", "PrivateNetwork"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("scope args must not contain %q: %q", forbidden, joined)
		}
	}
}

func TestReadVmHWMKB(t *testing.T) {
	if h := readVmHWMKB(os.Getpid()); h <= 0 {
		t.Fatalf("own VmHWM must be > 0, got %d", h)
	}
	if h := readVmHWMKB(999999); h != 0 {
		t.Fatalf("missing pid must read 0, got %d", h)
	}
}

func TestDescendantPIDsFindsChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep not available")
	}
	defer func() { _ = cmd.Process.Kill() }()
	found := false
	for _, pid := range descendantPIDs(os.Getpid()) {
		if pid == cmd.Process.Pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("descendantPIDs must find the sleep child %d", cmd.Process.Pid)
	}
}

func TestPeakPollerSamplesChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep not available")
	}
	defer func() { _ = cmd.Process.Kill() }()
	p := newPeakPoller(cmd.Process.Pid)
	time.Sleep(100 * time.Millisecond)
	if max := p.stopAndRead(); max <= 0 {
		t.Fatalf("poller must see a positive VmHWM for a live child, got %d", max)
	}
}

func TestCappedWriterTruncates(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cap")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &cappedWriter{f: f, limit: 10}
	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	if !w.truncated || w.written != 10 {
		t.Fatalf("writer must truncate at 10 bytes, got written=%d truncated=%v", w.written, w.truncated)
	}
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if w.written != 10 {
		t.Fatalf("writer must stay at 10 bytes after truncation, got %d", w.written)
	}
}

func TestCappedWriterRecordsWriteError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cap")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	// Close the file, then write through the writer: the underlying
	// write must fail and be recorded instead of blamed on the child.
	f2, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	w := &cappedWriter{f: f2, limit: 1024}
	if _, err := w.Write([]byte("hello")); err == nil {
		t.Fatalf("write to a closed file must return an error")
	}
	if w.writeErr == nil {
		t.Fatalf("writer must record the underlying write error")
	}
}

func TestGCNeverRemovesYoungLease(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "young")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// lease.json with a fresh start time.
	lease := &Lease{ID: "young", Tool: "/bin/true", Start: time.Now()}
	if err := writeLeaseJSON(filepath.Join(dir, "lease.json"), lease); err != nil {
		t.Fatal(err)
	}
	res, err := GC(root, 3600, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("young lease must not be removed, got %v", res.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("young lease dir must still exist: %v", err)
	}
}

func TestGCRemovesOldLease(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "old")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lease := &Lease{ID: "old", Tool: "/bin/true", Start: time.Now().Add(-2 * time.Hour)}
	if err := writeLeaseJSON(filepath.Join(dir, "lease.json"), lease); err != nil {
		t.Fatal(err)
	}
	res, err := GC(root, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "old" {
		t.Fatalf("old lease must be removed, got %v", res.Removed)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("old lease dir must be gone")
	}
}

func TestErrorJSONShape(t *testing.T) {
	f := NewError("BWRAP_MISSING", "bwrap was not found", "Install bubblewrap.", "Try again after installing.")
	if f.OK {
		t.Fatal("failure must have ok=false")
	}
	if f.Error.Code != "BWRAP_MISSING" || f.Error.Fix == "" || f.Error.IfThatFails == "" {
		t.Fatalf("error object incomplete: %+v", f.Error)
	}
}

func TestBwrapArgsProcFallback(t *testing.T) {
	o := RunOptions{Network: "none", TTL: 60, Memory: "2G", CPUs: 2}
	withProc := strings.Join(buildBwrapArgs(o, "/tmp/root/abc", "/tmp/tool", nil, "abc", true), " ")
	if !strings.Contains(withProc, "--proc /proc") {
		t.Errorf("useProc=true must mount a fresh proc: %q", withProc)
	}
	withoutProc := strings.Join(buildBwrapArgs(o, "/tmp/root/abc", "/tmp/tool", nil, "abc", false), " ")
	if !strings.Contains(withoutProc, "--dir /proc") || strings.Contains(withoutProc, "--proc /proc") {
		t.Errorf("useProc=false must use an empty /proc dir: %q", withoutProc)
	}
}
