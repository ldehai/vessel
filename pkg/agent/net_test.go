package agent

import (
	"reflect"
	"testing"
)

func TestNetCommands(t *testing.T) {
	cmds, err := netCommands(&NetConfig{
		Device: "eth0", IP: "10.88.0.5/16", Gateway: "10.88.0.1", MTU: 1450,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ip", "addr", "add", "10.88.0.5/16", "dev", "eth0"},
		{"ip", "link", "set", "eth0", "up"},
		{"ip", "link", "set", "eth0", "mtu", "1450"},
		{"ip", "route", "add", "default", "via", "10.88.0.1", "dev", "eth0"},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds = %v\nwant  %v", cmds, want)
	}
}

func TestNetCommandsOptionalPieces(t *testing.T) {
	// No gateway / no MTU: no route or mtu commands emitted.
	cmds, err := netCommands(&NetConfig{Device: "eth0", IP: "10.0.0.2/24"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("cmds = %v, want addr+up only", cmds)
	}
}

func TestNetCommandsValidation(t *testing.T) {
	if _, err := netCommands(nil); err == nil {
		t.Fatal("nil config must error")
	}
	if _, err := netCommands(&NetConfig{Device: "eth0"}); err == nil {
		t.Fatal("missing IP must error")
	}
	if _, err := netCommands(&NetConfig{IP: "10.0.0.2/24"}); err == nil {
		t.Fatal("missing device must error")
	}
}

// The op round-trips over the wire protocol; a bad config surfaces as a
// remote error, not a transport failure.
func TestConfigureNetOverProtocol(t *testing.T) {
	c := pipeClient(t)
	err := c.ConfigureNet(&NetConfig{Device: "", IP: ""})
	if err == nil {
		t.Fatal("invalid net config must return remote error")
	}
}
