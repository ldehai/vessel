//go:build !linux

package process

import (
	"context"
	"errors"

	"github.com/andyliu/vessel/pkg/sandbox"
)

// Driver is a stub on non-Linux platforms: namespace isolation requires
// the Linux kernel. On macOS, develop against a Linux VM or use `vessel`
// inside the CI container.
type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "process" }

func (d *Driver) Create(context.Context, *sandbox.Spec) (sandbox.Instance, error) {
	return nil, errors.New("process driver requires Linux (namespaces); on macOS run inside a Linux VM")
}
