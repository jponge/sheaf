//go:build !unix

package ops

import "context"

// lockBundle does not lock outside Unix: concurrent sheaf processes may lose
// each other's sheaf.yaml updates there (updates within one process are
// still serialized by Workspace).
func lockBundle(context.Context, string, func()) (func(), error) { return func() {}, nil }

// lockFileName is the name of the Unix lock file; it only appears in
// messages here, since no lock is taken.
const lockFileName = ".sheaf.lock"
