//go:build unix

package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockFileName is the lock file serializing sheaf.yaml updates between
// processes.
const lockFileName = "sheaf.yaml.lock"

// Lock polling: interval between attempts, and delay before waiting is
// reported.
const (
	lockPoll   = 100 * time.Millisecond
	lockNotify = 500 * time.Millisecond
)

// lockBundle takes an exclusive flock(2) on <root>/sheaf.yaml.lock and
// returns the function releasing it. While another process holds it, it
// retries every lockPoll, calls waiting once after lockNotify, and gives up
// with ctx.Err() when ctx is done. The file is left in place (removing it
// would let two processes lock different files).
func lockBundle(ctx context.Context, root string, waiting func()) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, lockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	start, notified := time.Now(), false
	for {
		var ferr error
		if err := rc.Control(func(fd uintptr) { ferr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB) }); err != nil {
			f.Close()
			return nil, err
		}
		if ferr == nil {
			return func() { f.Close() }, nil // closing releases the lock
		}
		if !errors.Is(ferr, syscall.EWOULDBLOCK) && !errors.Is(ferr, syscall.EINTR) {
			f.Close()
			return nil, ferr
		}
		if !notified && waiting != nil && time.Since(start) >= lockNotify {
			notified = true
			waiting()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
