package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GCResult is one removed lease, or the summary when --json is used.
type GCResult struct {
	Removed []string `json:"removed"`
}

// GC removes every lease under root older than olderThan seconds. A lease
// still running is killed first. It never removes a younger lease, and it
// exits 0 even when nothing was removed.
func GC(root string, olderThan int, asJSON bool) (GCResult, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			if asJSON {
				b, _ := json.Marshal(GCResult{Removed: []string{}})
				fmt.Println(string(b))
			}
			return GCResult{Removed: []string{}}, nil
		}
		return GCResult{}, err
	}
	cutoff := time.Now().Add(-time.Duration(olderThan) * time.Second)
	removed := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		l, err := ReadLease(filepath.Join(dir, "lease.json"))
		var start time.Time
		if err == nil {
			start = l.Start
		} else if st, serr := os.Stat(dir); serr == nil {
			start = st.ModTime()
		} else {
			continue
		}
		if !start.Before(cutoff) {
			continue
		}
		if l != nil {
			if Alive(l.PID) {
				KillGroup(l.PGID)
			}
			// Give the kill a moment, then check again.
			time.Sleep(100 * time.Millisecond)
		}
		if err := os.RemoveAll(dir); err != nil {
			return GCResult{Removed: removed}, fmt.Errorf("cannot remove %s: %w", dir, err)
		}
		removed = append(removed, e.Name())
		if !asJSON {
			fmt.Println("removed", e.Name())
		}
	}
	if asJSON {
		b, _ := json.Marshal(GCResult{Removed: removed})
		fmt.Println(string(b))
	}
	return GCResult{Removed: removed}, nil
}

// StatusEntry is one lease in `lease status` output.
type StatusEntry struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	PID     int    `json:"pid"`
	Start   string `json:"start"`
	TTL     int    `json:"ttl"`
	Network string `json:"network"`
	Out     string `json:"out"`
	Alive   bool   `json:"alive"`
}

// Status lists every lease under root.
func Status(root string, asJSON bool) ([]StatusEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			if asJSON {
				fmt.Println("[]")
			}
			return []StatusEntry{}, nil
		}
		return nil, err
	}
	out := []StatusEntry{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		l, err := ReadLease(filepath.Join(root, e.Name(), "lease.json"))
		if err != nil {
			continue
		}
		out = append(out, StatusEntry{
			ID: l.ID, Tool: l.Tool, PID: l.PID,
			Start: l.Start.Format(time.RFC3339), TTL: l.TTL,
			Network: l.Network, Out: l.Out, Alive: Alive(l.PID),
		})
	}
	if asJSON {
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	} else {
		for _, s := range out {
			fmt.Printf("%s tool=%s pid=%d start=%s ttl=%d network=%s alive=%v out=%s\n",
				s.ID, s.Tool, s.PID, s.Start, s.TTL, s.Network, s.Alive, s.Out)
		}
	}
	return out, nil
}
