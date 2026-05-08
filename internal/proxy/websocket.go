// Package proxy — websocket.go is a small RFC-6455 WebSocket
// implementation used by the terminal endpoint. We deliberately keep
// this in-tree (no third-party dep) so vajra-proxy stays a stdlib-only
// binary; the surface we need is small (server upgrade, server-side
// frame I/O, ping/close), and the protocol is stable.
package proxy

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
)

// wsGUID is the magic string defined by RFC 6455 §1.3.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcode values per RFC 6455 §11.8.
const (
	OpcodeContinuation byte = 0x0
	OpcodeText         byte = 0x1
	OpcodeBinary       byte = 0x2
	OpcodeClose        byte = 0x8
	OpcodePing         byte = 0x9
	OpcodePong         byte = 0xA
)

// WSConn is a minimal wrapper around the hijacked TCP connection: it
// reads inbound frames, writes outbound frames, and exposes the raw
// byte streams above the protocol. Single-goroutine reader and writer
// only — concurrent ReadFrame or concurrent WriteFrame is undefined.
type WSConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

// Frame is one decoded WebSocket frame as observed by the server.
// Servers should respect Final / Opcode but rarely care about the mask
// state — we strip masking before returning Payload.
type Frame struct {
	Final   bool
	Opcode  byte
	Payload []byte
}

// Upgrade completes the RFC 6455 §4.2 server handshake: validates the
// inbound HTTP upgrade request, computes the Sec-WebSocket-Accept
// value, hijacks the TCP connection, and writes the 101 response.
// On success the returned *WSConn owns the underlying connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing Upgrade: websocket")
	}
	if !headerContains(r.Header, "Connection", "upgrade") {
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
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write upgrade: %w", err)
	}
	return &WSConn{conn: conn, br: brw.Reader, bw: brw.Writer}, nil
}

// headerContains reports whether the comma-separated header field
// contains the (case-insensitive) value, with optional whitespace.
func headerContains(h http.Header, name, want string) bool {
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

// acceptKey computes the Sec-WebSocket-Accept response value for the
// client's Sec-WebSocket-Key per RFC 6455 §4.2.2.
func acceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ReadFrame reads one frame, transparently joining continuation frames
// for fragmented messages. Control frames (ping/pong/close) are
// returned with the data frames so callers can distinguish — pong is
// not auto-sent in response to a ping; the terminal handler issues
// pongs explicitly when it sees a ping.
func (c *WSConn) ReadFrame() (Frame, error) {
	return readFrame(c.br)
}

// WriteFrame writes a single frame with the requested opcode and the
// given payload. Server frames are never masked per RFC 6455 §5.3.
func (c *WSConn) WriteFrame(opcode byte, payload []byte) error {
	return writeFrame(c.bw, opcode, payload)
}

// WriteText is a convenience wrapper around WriteFrame for text payloads.
func (c *WSConn) WriteText(p []byte) error { return c.WriteFrame(OpcodeText, p) }

// WriteBinary is a convenience wrapper for binary payloads.
func (c *WSConn) WriteBinary(p []byte) error { return c.WriteFrame(OpcodeBinary, p) }

// WritePong replies to a client ping. Per RFC 6455 §5.5.3 the payload
// must equal the ping's payload.
func (c *WSConn) WritePong(payload []byte) error { return c.WriteFrame(OpcodePong, payload) }

// WriteClose sends a close frame with the given code and reason.
func (c *WSConn) WriteClose(code uint16, reason string) error {
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf, code)
	copy(buf[2:], reason)
	return c.WriteFrame(OpcodeClose, buf)
}

// Close shuts down the underlying TCP connection. Callers should
// usually call WriteClose first to give the peer a normal-shutdown
// indication.
func (c *WSConn) Close() error { return c.conn.Close() }

// readFrame implements the wire format. We support fragmentation for
// data frames (the terminal payload is small but the spec allows it);
// control frames must not be fragmented per §5.4.
func readFrame(r *bufio.Reader) (Frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	final := hdr[0]&0x80 != 0
	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// 16 MiB cap so a malicious client can't allocate a multi-GB buffer
	// per frame. Terminal payloads are kilobytes at most.
	if length > 16*1024*1024 {
		return Frame{}, fmt.Errorf("ws: frame too large: %d", length)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return Frame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return Frame{Final: final, Opcode: opcode, Payload: payload}, nil
}

// writeFrame writes one server-originated frame. Per spec, the server
// must not mask outbound frames.
func writeFrame(w *bufio.Writer, opcode byte, payload []byte) error {
	hdr := make([]byte, 2, 10)
	hdr[0] = 0x80 | (opcode & 0x0f) // FIN=1, RSVx=0
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
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return w.Flush()
}
