//go:build !unix

package ops

import "testing"

// holdLock skips outside Unix: sheaf.yaml is not locked there (see
// lockBundle), so lock-contention tests do not apply.
func holdLock(t *testing.T, _ string) (release func()) {
	t.Helper()
	t.Skip("sheaf.yaml locking is Unix-only")
	return func() {}
}
