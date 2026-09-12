package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// acceptWS upgrades a request.
//
// The origin check is the library's default: Origin's host must equal the
// request's Host. That is exactly what design section 10 relies on when it
// says the proxy forwards the original Host header so the websocket origin
// check keeps working.
//
// Compression is off. The framebuffer stream is already compressed by its RFB
// encoding, and serial output is small and latency sensitive, so negotiating
// per-message deflate would cost CPU on the hypervisor for nothing.
func acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
}

// wsStatusMessage is a status frame's payload. Status travels as text and VM
// output as binary, so a status message can never be mistaken for something
// the machine printed (design section 11).
type wsStatusMessage any

// writeStatus sends a text frame.
func writeStatus(ctx context.Context, c *websocket.Conn, msg wsStatusMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}

// writeOutput sends a binary frame.
func writeOutput(ctx context.Context, c *websocket.Conn, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageBinary, data)
}

const (
	// wsWriteTimeout bounds a single frame write, so one wedged client does
	// not pin a goroutine and a broker subscription forever.
	wsWriteTimeout = 10 * time.Second

	// wsReadLimit caps a client frame. Serial input is keystrokes and RFB
	// input is small messages; a megabyte is already far more than either
	// needs, and a clipboard paste is bounded by it rather than by memory.
	wsReadLimit = 1 << 20
)
