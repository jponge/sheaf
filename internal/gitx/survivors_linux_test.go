//go:build linux

package gitx

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// survivors returns the processes whose command line contains marker, and
// their parents when those are git processes (e.g. a fetch waiting for a
// fake ssh).
func survivors(marker string) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !strings.Contains(string(b), marker) || !processAlive(pid) {
			continue
		}
		out = append(out, pid)
		if ppid, _, ok := procStat(pid); ok && ppid > 1 {
			if pb, err := os.ReadFile("/proc/" + strconv.Itoa(ppid) + "/cmdline"); err == nil && strings.HasPrefix(string(pb), "git\x00") {
				out = append(out, ppid)
			}
		}
	}
	return out
}

// checkNoSurvivors fails the test at cleanup if processes started from
// marker (a fake ssh path) or the git processes waiting for them are still
// alive after a short grace period, and kills them.
func checkNoSurvivors(t *testing.T, marker string) {
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			left := survivors(marker)
			if len(left) == 0 {
				return
			}
			if time.Now().After(deadline) {
				for _, p := range left {
					_ = syscall.Kill(p, syscall.SIGKILL)
				}
				t.Errorf("processes %v outlived the test (killed)", left)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
