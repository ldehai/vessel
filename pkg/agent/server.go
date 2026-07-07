package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
)

// Server runs inside the guest (or a test) and executes host requests.
type Server struct{}

func NewServer() *Server { return &Server{} }

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return // EOF or bad frame: drop connection
		}
		resp := s.handle(&req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) handle(req *Request) *Response {
	resp := &Response{ID: req.ID}
	switch req.Op {
	case OpPing:
		// no-op: reaching here proves liveness
	case OpExec:
		s.execCmd(req, resp)
	case OpWriteFile:
		mode := os.FileMode(req.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
			resp.Err = err.Error()
			break
		}
		if err := os.WriteFile(req.Path, req.Data, mode); err != nil {
			resp.Err = err.Error()
		}
	case OpReadFile:
		data, err := os.ReadFile(req.Path)
		if err != nil {
			resp.Err = err.Error()
			break
		}
		resp.Data = data
	default:
		resp.Err = "unknown op: " + string(req.Op)
	}
	return resp
}

func (s *Server) execCmd(req *Request, resp *Response) {
	if len(req.Cmd) == 0 {
		resp.Err = "empty cmd"
		return
	}
	var stdout, stderr bytes.Buffer
	c := exec.Command(req.Cmd[0], req.Cmd[1:]...)
	c.Stdout, c.Stderr = &stdout, &stderr
	c.Env = append(os.Environ(), req.Env...)
	err := c.Run()
	resp.Stdout, resp.Stderr = stdout.Bytes(), stderr.Bytes()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		resp.ExitCode = 0
	case errors.As(err, &exitErr):
		resp.ExitCode = exitErr.ExitCode()
	default:
		resp.Err = err.Error()
	}
}

// ServeConn handles a single pre-established connection (useful for tests
// and for transports where the host initiates the connection).
func (s *Server) ServeConn(ctx context.Context, conn io.ReadWriteCloser) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		for {
			var req Request
			if err := dec.Decode(&req); err != nil {
				return
			}
			if err := enc.Encode(s.handle(&req)); err != nil {
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		conn.Close()
	case <-done:
	}
}
