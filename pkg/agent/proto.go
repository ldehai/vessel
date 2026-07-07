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
	OpPing      Op = "ping"
	OpExec      Op = "exec"
	OpWriteFile Op = "write_file"
	OpReadFile  Op = "read_file"
)

// Request is sent host -> guest.
type Request struct {
	ID   string   `json:"id"`
	Op   Op       `json:"op"`
	Cmd  []string `json:"cmd,omitempty"`  // OpExec
	Env  []string `json:"env,omitempty"`  // OpExec, KEY=VAL
	Path string   `json:"path,omitempty"` // file ops
	Data []byte   `json:"data,omitempty"` // OpWriteFile (JSON base64-encodes)
	Mode uint32   `json:"mode,omitempty"` // OpWriteFile permissions
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
