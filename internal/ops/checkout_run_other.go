//go:build !unix

package ops

import (
	"os/exec"
	"time"
)

// coProcTree is a no-op outside Unix: cancellation kills the command process
// only (exec.CommandContext's default).
type coProcTree struct{}

func coPrepareTree(*exec.Cmd) *coProcTree { return &coProcTree{} }

func (*coProcTree) finish(time.Duration) {}

func (*coProcTree) terminate() {}

func coKillTree(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func (*coProcTree) leftovers() bool { return false }

// coUnlinkOpen does nothing outside Unix: an open file cannot be removed
// there; it is removed after replay.
func coUnlinkOpen(string) bool { return false }
