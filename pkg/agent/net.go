package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// netCommands is the iproute2 sequence that makes the guest adopt the
// pod's network identity. Pure function so tests can assert the plan
// without a privileged environment.
func netCommands(c *NetConfig) ([][]string, error) {
	if c == nil {
		return nil, errors.New("configure_net: missing net config")
	}
	if c.Device == "" || c.IP == "" {
		return nil, errors.New("configure_net: device and ip are required")
	}
	cmds := [][]string{
		{"ip", "addr", "add", c.IP, "dev", c.Device},
		{"ip", "link", "set", c.Device, "up"},
	}
	if c.MTU > 0 {
		cmds = append(cmds, []string{"ip", "link", "set", c.Device, "mtu", strconv.Itoa(c.MTU)})
	}
	if c.Gateway != "" {
		cmds = append(cmds, []string{"ip", "route", "add", "default", "via", c.Gateway, "dev", c.Device})
	}
	return cmds, nil
}

func configureNet(c *NetConfig) error {
	cmds, err := netCommands(c)
	if err != nil {
		return err
	}
	for _, cmd := range cmds {
		if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("configure_net %v: %w: %s", cmd, err, out)
		}
	}
	return nil
}
