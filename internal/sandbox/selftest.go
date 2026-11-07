package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The self-test fixtures are inlined here (go:embed cannot reach
// ../../fixtures), and mirror fixtures/tool-echo.sh and fixtures/input.log.
const fixtureToolEcho = `#!/bin/sh
# Fixture tool: prints its lease environment and its arguments, then cats
# the first input file. Used by the README example and by --self-test.
echo "LEASE_OUT=$LEASE_OUT"
echo "LEASE_INPUT_0=$LEASE_INPUT_0"
echo "args: $*"
if [ -f "$LEASE_INPUT_0" ]; then
  cat "$LEASE_INPUT_0"
fi
`

const fixtureInputLog = `2026-11-08T02:00:00Z level=info msg="hello from the fixture log"
2026-11-08T02:00:01Z level=error msg="something failed" code=42
2026-11-08T02:00:02Z level=info msg="done"
`

// TestResult is one --self-test case.
type TestResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// SelfTestResult is the --self-test summary.
type SelfTestResult struct {
	OK    bool         `json:"ok"`
	Tests []TestResult `json:"tests"`
}

type leaseRunner struct {
	exe string
}

func (r leaseRunner) run(args ...string) (string, string, int) {
	cmd := exec.Command(r.exe, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return outBuf.String(), errBuf.String(), code
}

func writeTool(dir, name, content string) string {
	p := filepath.Join(dir, name)
	_ = os.WriteFile(p, []byte(content), 0o755)
	return p
}

func tempRoot() string {
	d, err := os.MkdirTemp("", "lease-selftest-*")
	if err != nil {
		return filepath.Join(os.TempDir(), "lease-selftest-fallback")
	}
	return d
}

// SelfTest runs the six security tests from the spec, always against
// temporary roots and inputs under the test's own temp dir — never the
// real home. It exits 0 only when every expected result matches.
func SelfTest(exe string, asJSON bool) SelfTestResult {
	r := leaseRunner{exe: exe}
	results := []TestResult{}
	pass := func(name, detail string) { results = append(results, TestResult{Name: name, OK: true, Detail: detail}) }
	fail := func(name, detail string) {
		results = append(results, TestResult{Name: name, OK: false, Detail: detail})
	}

	// 1. A child that writes /etc/lease-test fails.
	{
		root, work := tempRoot(), tempRoot()
		defer os.RemoveAll(root)
		defer os.RemoveAll(work)
		tool := writeTool(work, "write-etc.sh", "#!/bin/sh\necho x > /etc/lease-test || exit 3\necho wrote-it\n")
		_, _, code := r.run("run", "--tool", tool, "--out", filepath.Join(work, "out"), "--root", root, "--ttl", "10", "--network", "none", "--json", "--")
		if code != 0 {
			if _, err := os.Stat("/etc/lease-test"); os.IsNotExist(err) {
				pass("write-etc-fails", fmt.Sprintf("child failed as expected (exit %d), no /etc/lease-test on host", code))
			} else {
				fail("write-etc-fails", "child failed but /etc/lease-test exists on the host")
			}
		} else {
			fail("write-etc-fails", "child unexpectedly succeeded writing /etc/lease-test")
		}
	}

	// 2. A child that reads the host user's ssh key fails.
	{
		root, work := tempRoot(), tempRoot()
		defer os.RemoveAll(root)
		defer os.RemoveAll(work)
		home, _ := os.UserHomeDir()
		tool := writeTool(work, "read-ssh.sh", "#!/bin/sh\ncat \""+home+"/.ssh/id_ed25519\"\n")
		_, _, code := r.run("run", "--tool", tool, "--out", filepath.Join(work, "out"), "--root", root, "--ttl", "10", "--network", "none", "--json", "--")
		if code != 0 {
			pass("read-host-ssh-fails", fmt.Sprintf("child failed as expected (exit %d)", code))
		} else {
			fail("read-host-ssh-fails", "child unexpectedly read the host ssh key path")
		}
	}

	// 3. A child that opens a TCP connection fails with --network none.
	{
		root, work := tempRoot(), tempRoot()
		defer os.RemoveAll(root)
		defer os.RemoveAll(work)
		tool := writeTool(work, "net.sh", "#!/bin/bash\nexec 3<>/dev/tcp/8.8.8.8/53 || exit 3\necho connected\n")
		_, _, code := r.run("run", "--tool", tool, "--out", filepath.Join(work, "out"), "--root", root, "--ttl", "10", "--network", "none", "--json", "--")
		if code != 0 {
			pass("network-none-fails", fmt.Sprintf("TCP connect failed as expected (exit %d)", code))
		} else {
			fail("network-none-fails", "child unexpectedly opened a TCP connection")
		}
	}

	// 4. A child that sleeps longer than --ttl is killed and timed_out.
	{
		root, work := tempRoot(), tempRoot()
		defer os.RemoveAll(root)
		defer os.RemoveAll(work)
		out, _, code := r.run("run", "--tool", "/usr/bin/sleep", "--out", filepath.Join(work, "out"), "--root", root, "--ttl", "2", "--network", "none", "--json", "--", "5")
		var res RunResult
		_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &res)
		if code == 3 && res.TimedOut {
			pass("ttl-kills", "sleep 5 with ttl 2 was killed, timed_out=true, exit 3")
		} else {
			fail("ttl-kills", fmt.Sprintf("expected exit 3 + timed_out=true, got exit %d timed_out=%v out=%q", code, res.TimedOut, out))
		}
	}

	// 5. gc removes the directory and leaves no process.
	{
		root, work := tempRoot(), tempRoot()
		defer os.RemoveAll(root)
		defer os.RemoveAll(work)
		cmd := exec.Command(exe, "run", "--tool", "/usr/bin/sleep", "--out", filepath.Join(work, "out"), "--root", root, "--ttl", "60", "--network", "none", "--json", "--", "30")
		_ = cmd.Start()
		// Wait for the lease directory to appear.
		var leaseDir string
		var lease *Lease
		for i := 0; i < 50; i++ {
			entries, _ := os.ReadDir(root)
			if len(entries) > 0 {
				leaseDir = filepath.Join(root, entries[0].Name())
				if l, err := ReadLease(filepath.Join(leaseDir, "lease.json")); err == nil {
					lease = l
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if lease == nil || leaseDir == "" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			fail("gc-removes", "no lease directory appeared for the running child")
		} else {
			gcOut, _, gcCode := r.run("gc", "--root", root, "--older-than", "0", "--json")
			_ = cmd.Wait()
			if _, err := os.Stat(leaseDir); os.IsNotExist(err) && gcCode == 0 && !Alive(lease.PID) {
				pass("gc-removes", fmt.Sprintf("gc removed %s, no process left (gc: %s)", lease.ID, strings.TrimSpace(gcOut)))
			} else {
				fail("gc-removes", fmt.Sprintf("lease dir still exists or process alive (gc exit %d, out %q)", gcCode, gcOut))
			}
		}
	}

	// 6. A child that prints LEASE_OUT and LEASE_INPUT_0 sees /out and /input/0.
	{
		root, work := tempRoot(), tempRoot()
		defer os.RemoveAll(root)
		defer os.RemoveAll(work)
		input := filepath.Join(work, "input.log")
		_ = os.WriteFile(input, []byte(fixtureInputLog), 0o644)
		tool := writeTool(work, "tool-echo.sh", fixtureToolEcho)
		out, _, code := r.run("run", "--tool", tool, "--input", input, "--out", filepath.Join(work, "out"), "--root", root, "--ttl", "10", "--network", "none", "--", "hello")
		if code == 0 && strings.Contains(out, "LEASE_OUT=/out") && strings.Contains(out, "LEASE_INPUT_0=/input/0") && strings.Contains(out, "hello from the fixture log") {
			pass("env-vars-visible", "child saw LEASE_OUT=/out, LEASE_INPUT_0=/input/0 and the fixture input")
		} else {
			fail("env-vars-visible", fmt.Sprintf("expected env vars and fixture output, got exit %d out=%q", code, out))
		}
	}

	ok := true
	for _, t := range results {
		if !t.OK {
			ok = false
		}
	}
	res := SelfTestResult{OK: ok, Tests: results}
	if asJSON {
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
	} else {
		for _, t := range res.Tests {
			mark := "PASS"
			if !t.OK {
				mark = "FAIL"
			}
			fmt.Printf("%s %s: %s\n", mark, t.Name, t.Detail)
		}
	}
	return res
}
