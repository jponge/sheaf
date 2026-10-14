//go:build !unix

package gitx

import (
	"os/exec"
	"time"
)

// procGroup is a no-op outside Unix: exec.CommandContext kills only the git
// process on cancellation.
type procGroup struct{ cmd *exec.Cmd }

func startGroup(cmd *exec.Cmd, _ time.Duration, _ bool) *procGroup { return &procGroup{cmd: cmd} }

// kill kills the git process (only) and reports whether it succeeded.
func (pg *procGroup) kill() bool { return pg.cmd.Process.Kill() == nil }

func (*procGroup) finish() {}

func (*procGroup) killStragglers(<-chan struct{}) {}
