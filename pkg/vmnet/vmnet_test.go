//go:build linux

package vmnet

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testNS spins up an unprivileged user+net namespace holding a sleeping
// process, and returns an NS whose prefix enters it. Inside a user netns
// we hold full capabilities, so ip/tc work without root — the same code
// paths production uses via nsenter --net=<cni-path>.
func testNS(t *testing.T) NS {
	t.Helper()
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}
	cmd := exec.Command("unshare", "--user", "--net", "--map-root-user", "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot create user netns: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	pid := cmd.Process.Pid
	ns := NS{Prefix: []string{
		"nsenter", "-t", fmt.Sprint(pid), "-U", "--preserve-credentials", "-n",
	}}
	// The sleep may not have entered the netns yet; poll until usable.
	for i := 0; i < 100; i++ {
		if _, err := ns.run("ip", "link", "show", "lo"); err == nil {
			return ns
		}
	}
	t.Skip("test netns never became usable")
	return NS{}
}

// mimicCNI builds what a CNI plugin leaves behind: an eth0 with an address
// and a default route (via a veth peer acting as the gateway side).
func mimicCNI(t *testing.T, ns NS) {
	t.Helper()
	for _, s := range [][]string{
		{"ip", "link", "add", "eth0", "type", "veth", "peer", "name", "gwside"},
		{"ip", "link", "set", "eth0", "mtu", "1450"},
		{"ip", "addr", "add", "10.88.0.5/16", "dev", "eth0"},
		{"ip", "link", "set", "eth0", "up"},
		{"ip", "link", "set", "gwside", "up"},
		{"ip", "route", "add", "default", "via", "10.88.0.1", "dev", "eth0"},
	} {
		if _, err := ns.run(s...); err != nil {
			t.Fatalf("mimicCNI %v: %v", s, err)
		}
	}
}

func TestReadConfig(t *testing.T) {
	ns := testNS(t)
	mimicCNI(t, ns)

	cfg, err := ns.ReadConfig("eth0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IP != "10.88.0.5/16" {
		t.Fatalf("IP = %q, want 10.88.0.5/16", cfg.IP)
	}
	if cfg.Gateway != "10.88.0.1" {
		t.Fatalf("Gateway = %q, want 10.88.0.1", cfg.Gateway)
	}
	if cfg.MTU != 1450 {
		t.Fatalf("MTU = %d, want 1450", cfg.MTU)
	}
	if len(cfg.MAC) != 17 { // aa:bb:cc:dd:ee:ff
		t.Fatalf("MAC = %q", cfg.MAC)
	}
}

func TestSetupTapMirrors(t *testing.T) {
	ns := testNS(t)
	mimicCNI(t, ns)

	if err := ns.SetupTap("eth0", "tap0"); err != nil {
		t.Fatal(err)
	}

	// tap exists and is administratively up.
	out, err := ns.run("ip", "-j", "link", "show", "tap0")
	if err != nil || !strings.Contains(out, `"UP"`) {
		t.Fatalf("tap0 not up: %v %s", err, out)
	}
	// Both mirred redirects are installed.
	for dev, target := range map[string]string{"eth0": "tap0", "tap0": "eth0"} {
		out, err := ns.run("tc", "filter", "show", "dev", dev, "ingress")
		if err != nil || !strings.Contains(out, "mirred") || !strings.Contains(out, target) {
			t.Fatalf("%s ingress mirror to %s missing: %v\n%s", dev, target, err, out)
		}
	}

	// Teardown removes the tap.
	if err := ns.TeardownTap("tap0"); err != nil {
		t.Fatal(err)
	}
	if _, err := ns.run("ip", "link", "show", "tap0"); err == nil {
		t.Fatal("tap0 still present after teardown")
	}
}

// Setup against a missing source device must fail loudly and roll the tap
// back rather than leaving a half-mirrored interface.
func TestSetupTapRollsBackOnFailure(t *testing.T) {
	ns := testNS(t)

	if err := ns.SetupTap("no-such-dev", "tap1"); err == nil {
		t.Fatal("want error for missing source device")
	}
	if _, err := ns.run("ip", "link", "show", "tap1"); err == nil {
		t.Fatal("tap1 left behind after failed setup")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
