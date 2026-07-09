// Package agent implements the host <-> guest control protocol.
//
// Transport: any net.Conn (vsock in production, unix socket / net.Pipe in
// dev and tests). Framing: newline-delimited JSON, one request in flight
// per connection. This is deliberately boring — the interesting work
// (snapshot, fork) happens at the VMM layer, not here.
package agent

// Op is the operation requested by the host.
type Op string

const (
	OpPing         Op = "ping"
	OpExec         Op = "exec"
	OpWriteFile    Op = "write_file"
	OpReadFile     Op = "read_file"
	OpConfigureNet Op = "configure_net"
)

// NetConfig tells the guest to adopt the pod's network identity on a NIC
// (see pkg/vmnet: the host mirrors the CNI veth into the VM's virtio-net).
type NetConfig struct {
	Device  string `json:"device"`            // guest interface, e.g. "eth0"
	IP      string `json:"ip"`                // CIDR
	Gateway string `json:"gateway,omitempty"` // default route, optional
	MTU     int    `json:"mtu,omitempty"`
}

// Request is sent host -> guest.
type Request struct {
	ID   string     `json:"id"`
	Op   Op         `json:"op"`
	Cmd  []string   `json:"cmd,omitempty"`  // OpExec
	Env  []string   `json:"env,omitempty"`  // OpExec, KEY=VAL
	Path string     `json:"path,omitempty"` // file ops
	Data []byte     `json:"data,omitempty"` // OpWriteFile (JSON base64-encodes)
	Mode uint32     `json:"mode,omitempty"` // OpWriteFile permissions
	Net  *NetConfig `json:"net,omitempty"`  // OpConfigureNet
}

// Response is sent guest -> host.
type Response struct {
	ID       string `json:"id"`
	Err      string `json:"err,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"` // OpExec
	Stdout   []byte `json:"stdout,omitempty"`    // OpExec
	Stderr   []byte `json:"stderr,omitempty"`    // OpExec
	Data     []byte `json:"data,omitempty"`      // OpReadFile
}
