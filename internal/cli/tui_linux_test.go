//go:build linux

package cli

import (
	"os"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY allocates a pseudo-terminal pair (not as controlling terminal); it
// skips the test when /dev/ptmx is unusable.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("SKIP: no pseudo-terminal available: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	ioctl := func(req uintptr, arg unsafe.Pointer) error {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), req, uintptr(arg)); e != 0 {
			return e
		}
		return nil
	}
	var unlock int32
	if err := ioctl(syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Skipf("SKIP: unlockpt: %v", err)
	}
	var n uint32
	if err := ioctl(syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		t.Skipf("SKIP: ptsname: %v", err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("SKIP: open pty slave: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

func TestUseTUIOnTerminal(t *testing.T) {
	_, s := openPTY(t)
	if !isTerminal(s) {
		t.Fatal("isTerminal(pty) = false")
	}
	if !UseTUI(s, s, false) {
		t.Fatal("UseTUI(pty, pty) = false")
	}
	if UseTUI(s, s, true) {
		t.Fatal("UseTUI(pty, pty, --no-tui) = true")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if UseTUI(s, w, false) || UseTUI(r, s, false) {
		t.Fatal("UseTUI with one non-terminal stream = true")
	}
}
