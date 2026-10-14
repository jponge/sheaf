//go:build !unix

package gitx

import (
	"os/exec"
	"time"
)

// procGroup is a no-op outside Unix: exec.CommandContext kills only the git
// process on cancellation.
type procGroup struct{}

func startGroup(*exec.Cmd, time.Duration) *procGroup { return &procGroup{} }

func (*procGroup) finish() {}
