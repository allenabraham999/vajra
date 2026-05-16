// Package master — websocket.go is a minimal, dependency-free RFC 6455
// server-side WebSocket implementation. It exists solely for the
// browser terminal endpoint (handlers_terminal.go); keeping it in-tree
// lets vajra-master stay a stdlib-only binary. The surface is small —
// upgrade, frame read/write, ping/close — and the protocol is stable.
package master

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// wsGUID is the magic value defined by RFC 6455 §1.3, concatenated with
// the client key to compute the handshake accept token.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes per RFC 6455 §11.8.
const (
	wsOpcodeText   byte = 0x1
	wsOpcodeBinary byte = 0x2
	wsOpcodeClose  byte = 0x8
	wsOpcodePing   byte = 0x9
	wsOpcodePong   byte = 0xA
)

// wsConn wraps a hijacked TCP connection with RFC 6455 framing. One
// goroutine may call readFrame and one (other) goroutine may call the
// write* methods; neither side is internally synchronised.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

// wsFrame is one decoded inbound frame with masking already stripped.
type wsFrame struct {
	Opcode  byte
	Payload []byte
}

// wsUpgrade completes the RFC 6455 §4.2 server handshake, hijacks the
// connection, and clears the http.Server's accept-time read/write
// deadlines — a terminal session is long-lived and would otherwise be
// killed by ReadTimeout/WriteTimeout.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing Upgrade: websocket")
	}
	if !wsHeaderContains(r.Header, "Connection", "upgrade") {
		return nil, errors.New("missing Connection: upgrade")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("unsupported Sec-WebSocket-Version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write upgrade: %w", err)
	}
	return &wsConn{conn: conn, br: brw.Reader, bw: brw.Writer}, nil
}

// wsHeaderContains reports whether the comma-separated header field
// contains the (case-insensitive) value, tolerating surrounding space.
func wsHeaderContains(h http.Header, name, want string) bool {
	want = strings.ToLower(want)
	for _, v := range h.Values(name) {
		for _, p := range strings.Split(v, ",") {
			if strings.ToLower(strings.TrimSpace(p)) == want {
				return true
			}
		}
	}
	return false
}

// wsAcceptKey computes the Sec-WebSocket-Accept value per RFC 6455
// §4.2.2.
func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readFrame reads a single frame. Control frames (ping/pong/close) are
// returned alongside data frames; the caller decides how to react.
func (c *wsConn) readFrame() (wsFrame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return wsFrame{}, err
	}
	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// 16 MiB cap so a hostile client can't force a multi-GB allocation;
	// terminal payloads are kilobytes at most.
	if length > 16*1024*1024 {
		return wsFrame{}, fmt.Errorf("ws: frame too large: %d", length)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return wsFrame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return wsFrame{Opcode: opcode, Payload: payload}, nil
}

// writeFrame writes a single server frame. Per RFC 6455 §5.3 server
// frames are never masked.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	hdr := make([]byte, 2, 10)
	hdr[0] = 0x80 | (opcode & 0x0f) // FIN=1
	switch n := len(payload); {
	case n < 126:
		hdr[1] = byte(n)
	case n < 1<<16:
		hdr[1] = 126
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(n))
		hdr = append(hdr, ext...)
	default:
		hdr[1] = 127
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append(hdr, ext...)
	}
	if _, err := c.bw.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.bw.Write(payload); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

// writeBinary sends payload as a binary data frame.
func (c *wsConn) writeBinary(p []byte) error { return c.writeFrame(wsOpcodeBinary, p) }

// writePong replies to a client ping; the payload must echo the ping's.
func (c *wsConn) writePong(p []byte) error { return c.writeFrame(wsOpcodePong, p) }

// writeClose sends a close frame with the given code and reason.
func (c *wsConn) writeClose(code uint16, reason string) error {
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf, code)
	copy(buf[2:], reason)
	return c.writeFrame(wsOpcodeClose, buf)
}

// close shuts down the underlying TCP connection.
func (c *wsConn) close() error { return c.conn.Close() }
