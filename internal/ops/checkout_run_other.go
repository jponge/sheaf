//go:build !unix

package ops

import (
	"os/exec"
	"time"
)

// procTree is a no-op outside Unix: cancellation kills the command process
// only (exec.CommandContext's default).
type procTree struct{}

func prepareTree(*exec.Cmd) *procTree { return &procTree{} }

func (*procTree) finish(time.Duration) {}

func (*procTree) terminate() {}

func killTree(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func (*procTree) leftovers() bool { return false }

// unlinkOpen does nothing outside Unix: an open file cannot be removed
// there; it is removed after replay.
func unlinkOpen(string) bool { return false }
