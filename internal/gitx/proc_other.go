//go:build !unix

package gitx

import "os/exec"

// procGroup is a no-op outside Unix: exec.CommandContext kills only the git
// process on cancellation.
type procGroup struct{}

func startGroup(*exec.Cmd) *procGroup { return &procGroup{} }

func (*procGroup) finish() {}
