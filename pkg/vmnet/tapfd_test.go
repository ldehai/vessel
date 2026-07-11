//go:build linux

package vmnet

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func isPermErr(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

// TestOpenTapFD creates a TAP inside a user netns and opens an fd bound to
// it from outside that netns (the caller stays in its own netns; only a
// throwaway thread enters). The fd must be valid and refer to a tap.
func TestOpenTapFD(t *testing.T) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("/dev/net/tun unavailable")
	}
	ns := testNS(t)
	if err := ns.SetupTapDev("tapfd0"); err != nil {
		t.Skipf("cannot create tap in test netns: %v", err)
	}

	// Resolve the test netns path via the sleeper's pid.
	pid := nsPid(t, ns)
	netnsPath := "/proc/" + pid + "/ns/net"

	f, err := OpenTapFD(netnsPath, "tapfd0")
	if err != nil {
		// setns(CLONE_NEWNET) into a netns owned by an unprivileged user
		// namespace needs CAP_SYS_ADMIN over that user ns, which this test
		// process lacks. Production runs as root over a plain pod netns
		// where this succeeds; kvm-e2e (root) covers the positive path.
		if isPermErr(err) {
			t.Skipf("setns needs privilege the test env lacks: %v", err)
		}
		t.Fatal(err)
	}
	defer f.Close()

	if f.Fd() == 0 {
		t.Fatal("got fd 0")
	}
	// A TUNSETIFF'd fd answers TUNGETIFF with the device name.
	ifr, err := unix.NewIfreq("")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlIfreq(int(f.Fd()), unix.TUNGETIFF, ifr); err != nil {
		t.Fatalf("TUNGETIFF on opened fd: %v", err)
	}
	if ifr.Name() != "tapfd0" {
		t.Fatalf("fd bound to %q, want tapfd0", ifr.Name())
	}
}

func TestOpenTapFDMissingDevice(t *testing.T) {
	ns := testNS(t)
	pid := nsPid(t, ns)
	if _, err := OpenTapFD("/proc/"+pid+"/ns/net", "no-such-tap"); err == nil {
		t.Fatal("want error binding to a nonexistent tap")
	}
}

func TestOpenTapFDBadNetns(t *testing.T) {
	if _, err := OpenTapFD("/proc/nonexistent/ns/net", "tap0"); err == nil {
		t.Fatal("want error for bad netns path")
	}
}

// SetupTapDev creates just a tap device (no mirroring) for fd tests.
func (n NS) SetupTapDev(tap string) error {
	_, err := n.run("ip", "tuntap", "add", tap, "mode", "tap")
	return err
}

// nsPid extracts the sleeper pid embedded in the test NS prefix
// (nsenter -t <pid> ...).
func nsPid(t *testing.T, ns NS) string {
	t.Helper()
	for i, a := range ns.Prefix {
		if a == "-t" && i+1 < len(ns.Prefix) {
			return ns.Prefix[i+1]
		}
	}
	t.Fatal("no pid in test NS prefix")
	return ""
}
