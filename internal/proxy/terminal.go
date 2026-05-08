// Package proxy — terminal.go bridges a browser-side WebSocket to the
// agent's hijacked terminal endpoint. The wire mapping is:
//
//   Browser (WebSocket)               Agent (HTTP/1.1 + bytes)
//   ─────────────────────────────────────────────────────────
//   binary frame ─ keystroke bytes ─→ frame { type=0x00, data }
//   text frame  {"resize":[r,c]}   ─→ frame { type=0x01, [r,c] }
//   ─ PTY output bytes              ←─ raw bytes
//   close frame                    ─→ TCP close
//
// We split the WS reader and the agent reader onto their own goroutines
// and use a small mutex around outbound WS writes (browser side) so a
// pong + a data frame don't interleave on the wire.
package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// terminalDialTimeout caps the agent CONNECT phase. Once we're tunneling
// bytes we don't impose a deadline.
const terminalDialTimeout = 30 * time.Second

// resizeMsg is the JSON shape the dashboard sends on a SIGWINCH. Cols
// and Rows are the new terminal dimensions.
type resizeMsg struct {
	Resize *[2]int `json:"resize,omitempty"`
}

// handleTerminal is the WebSocket entrypoint:
// `GET /v1/sandboxes/{id}/terminal?token=...&cols=80&rows=24&command=...`.
// It resolves the sandbox, validates the optional share token, opens
// the agent CONNECT tunnel, then runs the bridge.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()

	route, err := s.cfg.Resolver.Resolve(r.Context(), id)
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if route.State != "" && route.State != "RUNNING" {
		http.Error(w, "sandbox not running", http.StatusServiceUnavailable)
		return
	}
	if token := q.Get("token"); token != "" {
		if err := s.cfg.Shares.ValidateShare(r.Context(), id, token, 0); err != nil {
			http.Error(w, "share rejected", http.StatusForbidden)
			return
		}
	}

	agentConn, err := s.dialTerminalAgent(r.Context(), route, q)
	if err != nil {
		s.logger.Warn("terminal: agent dial", "sandbox", id, "err", err)
		http.Error(w, "agent unreachable", http.StatusBadGateway)
		return
	}
	defer agentConn.Close()

	ws, err := Upgrade(w, r)
	if err != nil {
		s.logger.Warn("terminal: ws upgrade", "err", err)
		return
	}
	defer ws.Close()

	bridgeTerminal(r.Context(), ws, agentConn, s.logger)
}

// dialTerminalAgent opens a TCP connection to the agent and performs the
// HTTP/1.1 Upgrade handshake on it. We don't use net/http.Client because
// it doesn't expose the post-101 raw bytes.
func (s *Server) dialTerminalAgent(ctx context.Context, route *SandboxRoute, q url.Values) (net.Conn, error) {
	u, err := url.Parse(route.AgentBaseURL)
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if !hostHasPort(addr) {
		addr = addr + ":80"
	}
	dialer := &net.Dialer{Timeout: terminalDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Build the path with the same query string the client sent so the
	// agent can pick up cols/rows/command.
	path := "/sandbox/" + url.PathEscape(route.SandboxID) + "/terminal"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: vajra-terminal\r\n" +
		"Authorization: Bearer " + route.AgentSecret + "\r\n" +
		"User-Agent: " + DefaultUpstreamUserAgent + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, errors.New("agent did not upgrade: " + resp.Status)
	}
	return &bufConn{Conn: conn, r: br}, nil
}

// bufConn is a net.Conn whose Read pulls through a bufio.Reader so any
// bytes the http library sniffed out of the upgrade response aren't
// dropped on the floor.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

// Read forwards to the buffered reader.
func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// hostHasPort reports whether h looks like host:port. Conservative — an
// IPv6 literal without an explicit port still returns false.
func hostHasPort(h string) bool {
	_, _, err := net.SplitHostPort(h)
	return err == nil
}

// bridgeTerminal copies bytes both directions between the browser
// WebSocket and the agent CONNECT tunnel. Resize messages on the WS
// side are translated into the agent's framed-resize format.
func bridgeTerminal(ctx context.Context, ws *WSConn, agent net.Conn, logger interface {
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}) {
	var (
		writeMu sync.Mutex // browser WS writes are serialized
		wg      sync.WaitGroup
	)
	wg.Add(2)
	// Browser → agent.
	go func() {
		defer wg.Done()
		for {
			frame, err := ws.ReadFrame()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					logger.Debug("ws read", "err", err)
				}
				_ = agent.Close()
				return
			}
			switch frame.Opcode {
			case OpcodeBinary:
				if err := writeAgentDataFrame(agent, frame.Payload); err != nil {
					return
				}
			case OpcodeText:
				if err := handleText(agent, frame.Payload); err != nil {
					return
				}
			case OpcodePing:
				writeMu.Lock()
				_ = ws.WritePong(frame.Payload)
				writeMu.Unlock()
			case OpcodeClose:
				writeMu.Lock()
				_ = ws.WriteClose(1000, "client closed")
				writeMu.Unlock()
				_ = agent.Close()
				return
			}
		}
	}()
	// Agent → browser.
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := agent.Read(buf)
			if n > 0 {
				writeMu.Lock()
				werr := ws.WriteBinary(buf[:n])
				writeMu.Unlock()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-ctx.Done():
		_ = agent.Close()
		_ = ws.Close()
		<-doneCh
	case <-doneCh:
	}
}

// handleText dispatches a JSON text frame from the browser. Today only
// resize is recognised; any other body is silently ignored so a future
// dashboard can add new control messages without breaking older proxy
// builds.
func handleText(agent net.Conn, payload []byte) error {
	var msg resizeMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil
	}
	if msg.Resize == nil {
		return nil
	}
	rows, cols := msg.Resize[0], msg.Resize[1]
	if rows <= 0 || cols <= 0 || rows > 1<<16 || cols > 1<<16 {
		return nil
	}
	frame := make([]byte, 1+4+4)
	frame[0] = 0x01 // resize
	binary.BigEndian.PutUint32(frame[1:5], 4)
	binary.BigEndian.PutUint16(frame[5:7], uint16(rows))
	binary.BigEndian.PutUint16(frame[7:9], uint16(cols))
	_, err := agent.Write(frame)
	return err
}

// writeAgentDataFrame wraps payload in the type/length envelope the
// guest expects. We avoid allocating a fresh slice for the header by
// writing it separately — Write on a TCP conn is buffered downstream.
func writeAgentDataFrame(agent net.Conn, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = 0x00 // data
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := agent.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := agent.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// _ = strconv keeps the import alive for tests that reach into this
// file's helpers. Not strictly needed today; drop if vet flags it.
var _ = strconv.Atoi
