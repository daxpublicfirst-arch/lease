package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// peak.go — measuring the child's peak memory without cgroups.
//
// The original implementation reported the launcher process's rusage
// (wait4 maxrss). That number is the memory of bwrap's parent, not of the
// tool: bwrap forks, the child execs the tool, and the parent only waits.
// The rusage lease returned was therefore small and unrelated to what the
// tool used.
//
// The correct kernel-level number is the cgroup's memory.peak, but it is
// only readable when the scope's cgroup is visible in this host's cgroup
// view — in containers without delegated controllers it is not (see
// limitsEnforced). So lease measures peak in two layers:
//
//  1. cgroupPeakKB (systemd.go): read memory.peak from the scope cgroup
//     after the run, when the cgroup is visible. This counts everything
//     the cgroup used, including page cache (files the tool read/wrote),
//     so it is the whole footprint, not just heap.
//  2. A poller (this file): while the run is in progress, sample the
//     VmHWM (peak resident set size) of the tool process and all of its
//     descendants, every 25 ms, and keep the maximum. This works on any
//     host, systemd or fallback, because it only needs /proc.
//
// The cgroup number wins when available; otherwise the poller number is
// used and peak_source records which one it was, so callers can tell a
// sampled per-process peak from a cgroup peak. A tool that allocates and
// exits in under one sample interval can be missed by the poller; that
// limitation is documented in docs/DECISIONS.md.

// readVmHWMKB reads the VmHWM line (peak RSS, KiB) of one process from
// /proc/<pid>/status. It returns 0 on any error.
func readVmHWMKB(pid int) int64 {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// descendantPIDs returns the PIDs of all descendants of root (children,
// grandchildren, ...) by walking /proc/*/stat. Processes that exit mid-walk
// are skipped.
func descendantPIDs(root int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// stat is "pid (comm) state ppid ..."; comm may contain spaces and
		// parens, so find the last ")".
		s := string(stat)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	var out []int
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// peakPoller samples VmHWM of a process tree until told to stop.
type peakPoller struct {
	root     int
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	maxKB    int64
}

// newPeakPoller starts sampling the tree rooted at pid.
func newPeakPoller(pid int) *peakPoller {
	p := &peakPoller{
		root:     pid,
		interval: 25 * time.Millisecond,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *peakPoller) loop() {
	defer close(p.done)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	p.sample() // sample immediately, before the first tick
	for {
		select {
		case <-p.stop:
			p.sample() // one last sample on the way out
			return
		case <-t.C:
			p.sample()
		}
	}
}

func (p *peakPoller) sample() {
	max := p.maxKB
	for _, pid := range descendantPIDs(p.root) {
		if h := readVmHWMKB(pid); h > max {
			max = h
		}
	}
	// The root itself is the launcher (bwrap or systemd-run's child); its
	// own VmHWM is not the tool's, but including it is harmless: it is
	// small, and a tool that execs in place shares the root's PID only
	// when the launcher execs directly, in which case VmHWM is the
	// tool's own peak.
	if h := readVmHWMKB(p.root); h > max {
		max = h
	}
	p.mu.Lock()
	p.maxKB = max
	p.mu.Unlock()
}

// stopAndRead stops the poller and returns the highest VmHWM seen.
func (p *peakPoller) stopAndRead() int64 {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxKB
}
