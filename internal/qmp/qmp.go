// Package qmp speaks QEMU's machine protocol on a machine's control socket.
//
// It carries two things and no more: keyboard input and screen capture
// (design section 12). Power stays with systemd, because the VMs are systemd
// units and a unit reports a crashed machine as failed rather than absent.
//
// A QMP socket takes one client at a time, like a serial chardev. The serial
// line needs a broker because output arrives whether or not anybody listens.
// QMP does not: every message is an answer to a request. So a session here
// lasts one exchange, and Manager serialises the exchanges of one machine.
package qmp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// maxLine bounds one QMP message. Every reply labview asks for is a few
// bytes, so this is a guard against a socket that is not QEMU at all.
const maxLine = 1 << 20

// Client is one QMP session on one control socket.
type Client struct {
	conn net.Conn
	sc   *bufio.Scanner
	enc  *json.Encoder
}

// message is the shape of every line QEMU writes. One of the four fields is
// set.
type message struct {
	QMP    json.RawMessage `json:"QMP"`
	Return json.RawMessage `json:"return"`
	Error  *CommandError   `json:"error"`
	Event  string          `json:"event"`
}

// CommandError is a refusal from QEMU itself, such as an unknown key name.
// It names the command and the parameter and never the socket, so the API
// may show it to a client where a transport error must stay in the log.
type CommandError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *CommandError) Error() string {
	if e.Class == "" {
		return e.Desc
	}
	return e.Class + ": " + e.Desc
}

// Dial opens a control socket and negotiates on it.
func Dial(ctx context.Context, network, address string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return New(ctx, conn)
}

// New negotiates on an open connection, so the returned client is ready for a
// command.
func New(ctx context.Context, conn net.Conn) (*Client, error) {
	c := &Client{conn: conn, sc: bufio.NewScanner(conn), enc: json.NewEncoder(conn)}
	c.sc.Buffer(make([]byte, 0, 8<<10), maxLine)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// QEMU greets first, and it runs no other command until
	// qmp_capabilities answers the greeting.
	greeting, err := c.read()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("reading the QMP greeting: %w", err)
	}
	if len(greeting.QMP) == 0 {
		c.Close()
		return nil, errors.New("the socket did not open with a QMP greeting")
	}
	if _, err := c.Execute(ctx, "qmp_capabilities", nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close ends the session.
func (c *Client) Close() error { return c.conn.Close() }

// Execute runs one command and returns its result.
func (c *Client) Execute(ctx context.Context, command string, arguments any) (json.RawMessage, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
	}

	req := map[string]any{"execute": command}
	if arguments != nil {
		req["arguments"] = arguments
	}
	if err := c.enc.Encode(req); err != nil {
		return nil, fmt.Errorf("sending %s: %w", command, err)
	}

	for {
		msg, err := c.read()
		if err != nil {
			return nil, fmt.Errorf("reading the reply to %s: %w", command, err)
		}
		// An event is an answer to nothing. A client that takes the next
		// line for its reply reads the wrong one for every command after
		// it, and the symptom looks exactly like screendump returning
		// before the file exists.
		if msg.Event != "" {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s failed: %w", command, msg.Error)
		}
		if msg.Return != nil {
			return msg.Return, nil
		}
	}
}

func (c *Client) read() (message, error) {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return message{}, err
		}
		return message{}, errors.New("the control socket closed")
	}
	var msg message
	if err := json.Unmarshal(c.sc.Bytes(), &msg); err != nil {
		return message{}, fmt.Errorf("the socket wrote a line that is not JSON: %w", err)
	}
	return msg, nil
}
