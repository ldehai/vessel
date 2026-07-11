//go:build linux

package vmnet

// OpenTapFD opens a file descriptor bound to an existing TAP device that
// lives inside a network namespace, WITHOUT moving the caller's VMM into
// that namespace. This is the key to reconciling pod networking with the
// prewarmed VMM pool: the fd is opened in the pod netns and handed to a
// generic host-netns VMM via SCM_RIGHTS, because an open fd transcends the
// namespace it was created in.
//
// Mechanism: a dedicated OS thread enters the target netns with setns(2)
// (a per-thread operation — hence runtime.LockOSThread and no return to
// the original netns; the thread is discarded), opens /dev/net/tun, and
// binds it to the named TAP with TUNSETIFF(IFF_TAP|IFF_NO_PI). The
// returned fd is owned by the caller.

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// OpenTapFD returns an fd bound to tap inside the netns at netnsPath.
// The caller owns the returned *os.File and must Close it once the VMM has
// duplicated it (after the restore/create call returns).
func OpenTapFD(netnsPath, tap string) (*os.File, error) {
	type result struct {
		f   *os.File
		err error
	}
	ch := make(chan result, 1)

	// Run on a locked thread that switches netns and is never unlocked, so
	// the Go runtime retires it instead of reusing it with a dirty netns.
	go func() {
		runtime.LockOSThread()

		host, err := currentNetnsFD()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		defer host.Close()

		target, err := os.Open(netnsPath)
		if err != nil {
			ch <- result{nil, fmt.Errorf("open netns %s: %w", netnsPath, err)}
			return
		}
		defer target.Close()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", netnsPath, err)}
			return
		}
		// Intentionally do NOT switch back and do NOT unlock the thread:
		// leaving it locked lets the runtime discard the tainted thread.

		f, err := openTapInCurrentNS(tap)
		ch <- result{f, err}
	}()

	r := <-ch
	return r.f, r.err
}

func currentNetnsFD() (*os.File, error) {
	f, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		// Older kernels lack thread-self; fall back to the task path.
		f, err = os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
		if err != nil {
			return nil, fmt.Errorf("open current netns: %w", err)
		}
	}
	return f, nil
}

// openTapInCurrentNS opens /dev/net/tun and binds it to an existing TAP by
// name in whatever netns the calling thread is currently in.
func openTapInCurrentNS(tap string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	ifr, err := unix.NewIfreq(tap)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	// IFF_TAP: L2 tap. IFF_NO_PI: no 4-byte packet-info prefix (what a
	// virtio-net backend expects). The device already exists (SetupTap
	// created it), so TUNSETIFF attaches to it rather than creating.
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %s: %w", tap, err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun:"+tap), nil
}
