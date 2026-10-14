//go:build unix && !linux

package gitx

import "syscall"

// procID is unused outside Linux: descendants of an interactive git
// invocation cannot be enumerated portably, so only git itself is signalled
// (see WithInteractive).
type procID struct{}

func descendants(int) []procID { return nil }

func (procID) alive() bool { return false }

func (procID) signal(syscall.Signal) {}
