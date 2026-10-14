//go:build linux

package gitx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// groupMembers returns the processes of process group pgid, from /proc.
func groupMembers(pgid int) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:]) // state ppid pgrp ...
		if len(f) > 2 && f[2] == strconv.Itoa(pgid) {
			out = append(out, pid)
		}
	}
	return out
}

// pidsIn returns the PIDs recorded in pidfile so far.
func pidsIn(pidfile string) []int {
	b, _ := os.ReadFile(pidfile)
	var out []int
	for f := range strings.FieldsSeq(string(b)) {
		if p, err := strconv.Atoi(f); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// A process forked while the group SIGKILL of Force is being delivered may
// miss it: Linux makes fork atomic with respect to process-group signals,
// other kernels may not (suspected on macOS, where the merge returned only
// after waitDelay). Simulated here: the first group SIGKILL spares the
// background child of the smudge filter, which holds git's stderr open. Force
// must still kill the whole group promptly.
func TestForceKillCatchesGroupMemberThatMissedTheFirstSignal(t *testing.T) {
	dir, pidfile := slowSmudgeClone(t)
	if err := Fetch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	force := make(chan struct{})
	go func() {
		defer close(force)
		for range 2000 {
			if len(pidsIn(pidfile)) >= 2 { // the filter and its background child
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	var (
		mu      sync.Mutex
		spared  bool
		escaped int
	)
	orig := killGroup
	killGroup = func(pgid int, sig syscall.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		if sig == syscall.SIGKILL && !spared {
			spared = true
			pids := pidsIn(pidfile)
			escaped = pids[len(pids)-1]
			for _, p := range groupMembers(pgid) {
				if p != escaped {
					_ = syscall.Kill(p, syscall.SIGKILL)
				}
			}
			return nil
		}
		return orig(pgid, sig)
	}
	t.Cleanup(func() { killGroup = orig })

	start := time.Now()
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{MergeTimeout: 20 * time.Second, Force: force})
	elapsed := time.Since(start)
	mu.Lock()
	if !spared || escaped == 0 {
		t.Fatal("setup: the simulated lost signal did not happen")
	}
	mu.Unlock()
	wantForceKilled(t, elapsed, r, pidfile)
}
