// Package vmnet bridges a CNI-configured pod network namespace into a
// microVM (the Kata tc-mirror approach):
//
//	pod netns:  eth0 (CNI veth) <--tc mirred--> tapX --virtio-net--> guest
//
// CNI has already placed eth0 with an IP in the pod netns; the VM must see
// that traffic. Inside the netns we create a TAP device, mirror packets
// both ways between eth0 and the TAP with tc, hand the TAP to the VMM, and
// tell the guest agent to configure eth0's IP/route on its virtio-net.
//
// Implementation note: operations shell out to iproute2 (ip/tc) run
// through an EnterNS command prefix. v1 favours debuggability and zero new
// dependencies over a netlink library; the exec layer is a seam both for
// testing (user-netns prefix) and a later netlink port.
package vmnet

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// Config is the pod network identity the guest must adopt.
type Config struct {
	IP      string `json:"ip"`      // CIDR, e.g. "10.88.0.5/16"
	Gateway string `json:"gateway"` // default route, may be empty
	MAC     string `json:"mac"`     // eth0's MAC; guest NIC clones it so CNI-side ARP/filters hold
	MTU     int    `json:"mtu"`
}

// NS runs commands inside one network namespace via a command prefix
// (production: {"nsenter", "--net=<path>"}; tests: an unprivileged
// user-netns prefix).
type NS struct {
	Prefix []string
}

// EnterNS is the production namespace entry for a CNI netns path.
func EnterNS(netnsPath string) NS {
	return NS{Prefix: []string{"nsenter", "--net=" + netnsPath}}
}

func (n NS) run(args ...string) (string, error) {
	full := append(append([]string{}, n.Prefix...), args...)
	out, err := exec.Command(full[0], full[1:]...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %w: %s", args, err, out)
	}
	return string(out), nil
}

// ReadConfig extracts dev's IP/gateway/MAC/MTU inside the netns.
func (n NS) ReadConfig(dev string) (*Config, error) {
	cfg := &Config{}

	linkJSON, err := n.run("ip", "-j", "link", "show", dev)
	if err != nil {
		return nil, err
	}
	var links []struct {
		Address string `json:"address"`
		MTU     int    `json:"mtu"`
	}
	if err := json.Unmarshal([]byte(linkJSON), &links); err != nil || len(links) == 0 {
		return nil, fmt.Errorf("parse link %s: %v", dev, err)
	}
	cfg.MAC, cfg.MTU = links[0].Address, links[0].MTU

	addrJSON, err := n.run("ip", "-j", "-4", "addr", "show", dev)
	if err != nil {
		return nil, err
	}
	var addrs []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(addrJSON), &addrs); err != nil {
		return nil, fmt.Errorf("parse addr %s: %v", dev, err)
	}
	if len(addrs) > 0 && len(addrs[0].AddrInfo) > 0 {
		ai := addrs[0].AddrInfo[0]
		cfg.IP = fmt.Sprintf("%s/%d", ai.Local, ai.PrefixLen)
	}

	routeJSON, err := n.run("ip", "-j", "route", "show", "default")
	if err == nil { // no default route is fine (isolated pods)
		var routes []struct {
			Gateway string `json:"gateway"`
		}
		if json.Unmarshal([]byte(routeJSON), &routes) == nil && len(routes) > 0 {
			cfg.Gateway = routes[0].Gateway
		}
	}
	return cfg, nil
}

// SetupTap creates tap inside the netns and cross-mirrors all packets
// between src (the CNI veth, usually eth0) and the tap:
//
//	src ingress  -> redirect egress tap   (world -> guest)
//	tap ingress  -> redirect egress src   (guest -> world)
//
// The tap is left up and ready to hand to the VMM by name.
func (n NS) SetupTap(src, tap string) error {
	steps := [][]string{
		{"ip", "tuntap", "add", tap, "mode", "tap"},
		{"ip", "link", "set", tap, "up"},
		{"tc", "qdisc", "add", "dev", src, "ingress"},
		{"tc", "filter", "add", "dev", src, "ingress", "protocol", "all",
			"u32", "match", "u8", "0", "0", "action", "mirred", "egress", "redirect", "dev", tap},
		{"tc", "qdisc", "add", "dev", tap, "ingress"},
		{"tc", "filter", "add", "dev", tap, "ingress", "protocol", "all",
			"u32", "match", "u8", "0", "0", "action", "mirred", "egress", "redirect", "dev", src},
	}
	for _, s := range steps {
		if _, err := n.run(s...); err != nil {
			_ = n.TeardownTap(tap) // best-effort rollback of partial setup
			return err
		}
	}
	return nil
}

// TeardownTap removes the tap (its tc rules die with it; the src qdisc is
// left in place — harmless, and the netns is torn down by CNI anyway).
func (n NS) TeardownTap(tap string) error {
	_, err := n.run("ip", "link", "del", tap)
	return err
}
