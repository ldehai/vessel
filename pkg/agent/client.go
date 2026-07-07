package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Client is the host-side handle to a guest agent connection.
type Client struct {
	mu   sync.Mutex // one request in flight per connection
	conn io.ReadWriteCloser
	dec  *json.Decoder
	enc  *json.Encoder
	seq  atomic.Uint64
}

func NewClient(conn io.ReadWriteCloser) *Client {
	return &Client{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) roundTrip(req *Request) (*Response, error) {
	req.ID = fmt.Sprintf("r%d", c.seq.Add(1))
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enc.Encode(req); err != nil {
		return nil, fmt.Errorf("agent send: %w", err)
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("agent recv: %w", err)
	}
	if resp.ID != req.ID {
		return nil, fmt.Errorf("agent: response id %s != request id %s", resp.ID, req.ID)
	}
	if resp.Err != "" {
		return &resp, errors.New(resp.Err)
	}
	return &resp, nil
}

func (c *Client) Ping() error {
	_, err := c.roundTrip(&Request{Op: OpPing})
	return err
}

// Exec runs cmd in the guest and returns exit code, stdout, stderr.
func (c *Client) Exec(cmd, env []string) (int, []byte, []byte, error) {
	resp, err := c.roundTrip(&Request{Op: OpExec, Cmd: cmd, Env: env})
	if err != nil {
		return -1, nil, nil, err
	}
	return resp.ExitCode, resp.Stdout, resp.Stderr, nil
}

func (c *Client) WriteFile(path string, data []byte, mode uint32) error {
	_, err := c.roundTrip(&Request{Op: OpWriteFile, Path: path, Data: data, Mode: mode})
	return err
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	resp, err := c.roundTrip(&Request{Op: OpReadFile, Path: path})
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}
