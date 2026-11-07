package sandbox

import (
	"os"
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
	args := buildBwrapArgs(o, "/tmp/root/abc", "/tmp/tool", []string{"/tmp/in.log"}, "abc")
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
	if strings.Contains(joined, "--ro-bind /bin /bin") || strings.Contains(joined, "--ro-bind /lib /lib") {
		t.Errorf("must not bind symlinked /bin or /lib separately: %q", joined)
	}
	if !strings.HasSuffix(joined, "/tool a b") {
		t.Errorf("tool + argv should come last: %q", joined)
	}
}

func TestBwrapArgsLocalhostBringsLoopbackUp(t *testing.T) {
	o := RunOptions{Network: "localhost", TTL: 60, Memory: "2G", CPUs: 2}
	args := buildBwrapArgs(o, "/tmp/root/abc", "/tmp/tool", nil, "abc")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ip link set lo up") {
		t.Errorf("localhost mode must bring lo up: %q", joined)
	}
}

func TestSystemdArgsUseRuntimeMaxSec(t *testing.T) {
	o := RunOptions{TTL: 60, Memory: "2G", CPUs: 2, Network: "none"}
	args := buildSystemdArgs(o, "abc", []string{"bwrap", "--ro-bind", "/usr", "/usr"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"RuntimeMaxSec=60s", "MemoryMax=2G", "CPUQuota=200%", "TasksMax=64", "NoNewPrivileges=yes", "PrivateNetwork=yes", "systemd-run --user --scope"} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemd args missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "RuntimeMax=60") {
		t.Errorf("must use RuntimeMaxSec, not RuntimeMax: %q", joined)
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
