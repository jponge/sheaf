//go:build linux

package gitx

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/jponge/sheaf/internal/testutil"
)

// openPTY allocates a pseudo-terminal pair without making it the
// controlling terminal of the test process. It skips the test when the
// environment has no usable /dev/ptmx.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("SKIP: no pseudo-terminal available (%v); cannot test /dev/tty isolation", err)
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
		t.Skipf("SKIP: unlockpt failed (%v); cannot test /dev/tty isolation", err)
	}
	var n uint32
	if err := ioctl(syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		t.Skipf("SKIP: ptsname failed (%v); cannot test /dev/tty isolation", err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("SKIP: cannot open pty slave (%v); cannot test /dev/tty isolation", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

// TestTTYHelperProcess is not a test: it is the body of the helper process
// started by TestGitCannotOpenControllingTerminal, which runs it with a
// pseudo-terminal as controlling terminal.
func TestTTYHelperProcess(t *testing.T) {
	dir := os.Getenv("GITX_TTY_HELPER_DIR")
	if dir == "" {
		return
	}
	sanity := os.Getenv("GITX_TTY_HELPER_SANITY")
	if f, err := os.Open("/dev/tty"); err == nil {
		f.Close()
		_ = os.WriteFile(sanity, []byte("opened"), 0o644)
	} else {
		_ = os.WriteFile(sanity, []byte("denied: "+err.Error()), 0o644)
	}
	_ = Net{}.FetchRemote(context.Background(), dir, "origin") // the fake ssh always fails
}

// Git (and the ssh it spawns) must not be able to open the controlling
// terminal of sheaf: an ssh password or host-key prompt on /dev/tty would
// corrupt the TUI and block forever.
func TestGitCannotOpenControllingTerminal(t *testing.T) {
	result := filepath.Join(t.TempDir(), "tty-result")
	dir := sshFakeClone(t, "if (exec 3</dev/tty) 2>/dev/null; then echo opened > '"+result+"'; else echo denied > '"+result+"'; fi\nexit 1\n")
	_, slave := openPTY(t)
	sanity := filepath.Join(t.TempDir(), "sanity")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTTYHelperProcess$", "-test.count=1")
	cmd.Env = testutil.WithoutRaceExitDelay(append(os.Environ(), "GITX_TTY_HELPER_DIR="+dir, "GITX_TTY_HELPER_SANITY="+sanity))
	cmd.Stdin = slave
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process: %v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(sanity); string(b) != "opened" {
		t.Skipf("SKIP: the helper process has no usable controlling terminal (%q); cannot test /dev/tty isolation", b)
	}
	b, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("fake ssh never ran: %v\n%s", err, out.String())
	}
	if got := strings.TrimSpace(string(b)); got != "denied" {
		t.Fatalf("ssh spawned by git could open /dev/tty (%q); prompts would hit the terminal", got)
	}
}
