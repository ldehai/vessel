package cloudhypervisor

// Pod networking for microVMs (the driver side of pkg/vmnet):
//
//	pod netns:  eth0 (CNI veth) <--tc mirred--> tapX --virtio-net--> guest eth0
//
// setupNetwork runs before vm.create: it reads the pod's network identity
// off the CNI veth, creates the mirrored TAP, and attaches it to the VM
// config. applyGuestNetwork runs after the agent is reachable: the guest
// adopts the pod's IP/route/MTU on its virtio NIC. The guest NIC clones
// the veth's MAC so CNI-side ARP entries and MAC filters keep matching.
//
// The VMM itself must live in the pod netns to open the TAP by name —
// that is spawnInNetns's job (and why netns'd pods bypass the prewarmed
// VMM pool).

import (
	"fmt"

	"github.com/ldehai/vessel/pkg/agent"
	"github.com/ldehai/vessel/pkg/vmnet"
)

// cniDev is the interface CNI plugins configure in the pod netns.
const cniDev = "eth0"

// guestDev is the guest-side virtio NIC name (first and only NIC).
const guestDev = "eth0"

// netSetup records what setupNetwork built, for guest config and teardown.
type netSetup struct {
	ns  vmnet.NS
	tap string
	cfg *vmnet.Config
}

// tapName derives a per-instance TAP name. IFNAMSIZ is 16, so use a short
// prefix + the 12-hex-char instance id's head.
func tapName(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return "vtap" + id
}

// setupNetwork bridges the pod netns into the VM config (host side).
func (i *Instance) setupNetwork(vmCfg *VMConfig) error {
	ns := vmnet.EnterNS(i.netns)

	cfg, err := ns.ReadConfig(cniDev)
	if err != nil {
		return fmt.Errorf("read %s in %s: %w", cniDev, i.netns, err)
	}
	if cfg.IP == "" {
		return fmt.Errorf("%s in %s has no IPv4 address (CNI not run?)", cniDev, i.netns)
	}

	tap := tapName(i.id)
	if err := ns.SetupTap(cniDev, tap); err != nil {
		return fmt.Errorf("mirror %s->%s: %w", cniDev, tap, err)
	}

	vmCfg.Net = append(vmCfg.Net, NetDevice{Tap: tap, MAC: cfg.MAC})
	i.netCfg = &netSetup{ns: ns, tap: tap, cfg: cfg}
	return nil
}

// applyGuestNetwork makes the guest adopt the pod's network identity
// (guest side; requires the agent connection).
func (i *Instance) applyGuestNetwork() error {
	if i.netCfg == nil {
		return nil
	}
	return i.agent.ConfigureNet(&agent.NetConfig{
		Device:  guestDev,
		IP:      i.netCfg.cfg.IP,
		Gateway: i.netCfg.cfg.Gateway,
		MTU:     i.netCfg.cfg.MTU,
	})
}

// teardownNetwork removes the TAP (best-effort: the netns usually dies
// with the pod anyway).
func (i *Instance) teardownNetwork() {
	if i.netCfg == nil {
		return
	}
	_ = i.netCfg.ns.TeardownTap(i.netCfg.tap)
	i.netCfg = nil
}
