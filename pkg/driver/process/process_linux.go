//go:build linux

// Package process implements the development driver: Linux namespace
// isolation without a VM. It exists so the sandbox API, session model and
// CLI can be built and tested before the microVM drivers land (M2).
//
// Isolation provided: user, PID, mount, UTS and IPC namespaces.
// NOT provided: independent kernel. Do not use for untrusted code in
// production — that is what the VM drivers are for.
package process

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/andyliu/vessel/pkg/sandbox"
)

type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "process" }

func (d *Driver) Create(_ context.Context, spec *sandbox.Spec) (sandbox.Instance, error) {
	return &instance{id: sandbox.NewID(), spec: spec, state: sandbox.StateRunning}, nil
}

type instance struct {
	id    string
	spec  *sandbox.Spec
	state sandbox.State
}

func (i *instance) ID() string           { return i.id }
func (i *instance) State() sandbox.State { return i.state }

func (i *instance) Exec(ctx context.Context, cmd []string, stdout, stderr io.Writer) (int, error) {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Stdout, c.Stderr = stdout, stderr
	c.Env = envSlice(i.spec.Env)
	c.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}
	if i.spec.Rootfs != "" {
		c.SysProcAttr.Chroot = i.spec.Rootfs
	}
	err := c.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (i *instance) Snapshot(context.Context, string) error {
	return sandbox.ErrNotSupported
}

func (i *instance) Stop(context.Context) error {
	i.state = sandbox.StateStopped
	return nil
}

func envSlice(m map[string]string) []string {
	env := os.Environ()
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	return env
}
