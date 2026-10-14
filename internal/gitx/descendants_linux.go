//go:build linux

package gitx

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procID identifies a process by pid and start time, so that a recorded
// process is not confused with a later one reusing its pid.
type procID struct {
	pid   int
	start string
}

// procStat returns the parent pid and start time of pid from
// /proc/<pid>/stat.
func procStat(pid int) (ppid int, start string, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, "", false
	}
	f := strings.Fields(s[i+1:]) // state ppid pgrp session ... starttime is field 22 overall
	if len(f) < 20 {
		return 0, "", false
	}
	ppid, err = strconv.Atoi(f[1])
	if err != nil {
		return 0, "", false
	}
	return ppid, f[19], true
}

// descendants returns the processes below root (children, grandchildren,
// ...) found by scanning /proc, best effort: a process that was reparented
// (its parent exited) before the scan is not found.
func descendants(root int) []procID {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]procID{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, start, ok := procStat(pid); ok {
			children[ppid] = append(children[ppid], procID{pid, start})
		}
	}
	var out []procID
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

// alive reports whether the recorded process still exists (same start time)
// and is not a zombie.
func (p procID) alive() bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(p.pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) || s[i+2] == 'Z' {
		return false
	}
	_, start, ok := procStat(p.pid)
	return ok && start == p.start
}

// signal sends sig to the recorded process if it is still the same process.
func (p procID) signal(sig syscall.Signal) {
	if _, start, ok := procStat(p.pid); ok && start == p.start {
		_ = syscall.Kill(p.pid, sig)
	}
}
