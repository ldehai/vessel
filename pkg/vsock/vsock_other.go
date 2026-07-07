//go:build !linux

package vsock

import (
	"errors"
	"net"
)

const (
	CIDHypervisor = 0
	CIDLocal      = 1
	CIDHost       = 2
)

var errUnsupported = errors.New("vsock requires Linux")

func Dial(cid, port uint32) (net.Conn, error)  { return nil, errUnsupported }
func Listen(port uint32) (net.Listener, error) { return nil, errUnsupported }
