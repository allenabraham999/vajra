// Package master — handlers_terminal.go serves the browser terminal at
// GET /v1/sandboxes/{id}/terminal. The dashboard opens a WebSocket
// here; master upgrades it, dials the owning agent's hijack-style
// terminal endpoint, and bridges bytes between the two.
//
// Wire mapping (matches vajra-proxy's terminal bridge and the guest
// agent's frame parser):
//
//	Browser (WebSocket)               Agent / guest (raw bytes)
//	──────────────────────────────────────────────────────────
//	binary frame ─ keystrokes ──────→ frame {0x00, uint32 len, data}
//	text frame  {"resize":[r,c]} ───→ frame {0x01, uint32 4, r, c}
//	─ PTY output ──────────────────←─ raw bytes → WS binary frame
//
// The route is registered OUTSIDE AuthMiddleware: a browser WebSocket
// cannot send an Authorization header, so the caller passes its JWT (or
// API key) as the ?token= query parameter and the handler validates it
// itself.
package master

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// terminalDialTimeout caps the agent CONNECT handshake. Once bytes are
// flowing we impose no deadline — the session runs until either side
// closes.
const terminalDialTimeout = 30 * time.Second

// terminalSandbox is the WebSocket entrypoint for the dashboard
// terminal. It authenticates from ?token=, resolves the sandbox + node,
// dials the agent, then bridges the two streams.
func (h *Handlers) terminalSandbox(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if id == "" {
		http.Error(w, "missing sandbox id", http.StatusBadRequest)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	accountID, status := resolveToken(r.Context(), h.Signer, h.Store.APIKeys(), token)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	sb, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, id)
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if sb.State != models.SandboxStateRunning {
		http.Error(w, "sandbox is "+string(sb.State)+", not RUNNING", http.StatusConflict)
		return
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		http.Error(w, "sandbox has no placement", http.StatusConflict)
		return
	}
	node, err := h.Store.Nodes().GetByID(r.Context(), *sb.NodeID)
	if err != nil {
		h.log().Error("terminal: load node", "err", err, "sandbox_id", id)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Dial the agent BEFORE upgrading the browser side: while we still
	// hold a normal ResponseWriter we can return a clean HTTP error if
	// the agent is unreachable.
	agentConn, err := h.dialAgentTerminal(r.Context(), node, id, r.URL.Query())
	if err != nil {
		h.log().Warn("terminal: agent dial", "err", err, "sandbox_id", id)
		http.Error(w, "agent unreachable", http.StatusBadGateway)
		return
	}
	defer agentConn.Close()

	ws, err := wsUpgrade(w, r)
	if err != nil {
		h.log().Warn("terminal: ws upgrade", "err", err, "sandbox_id", id)
		return
	}
	defer ws.close()

	h.touchActivity(r.Context(), id)
	h.log().Info("terminal: session opened", "sandbox_id", id, "account_id", accountID)
	bridgeTerminal(r.Context(), ws, agentConn)
}

// dialAgentTerminal opens a raw TCP connection to the node agent and
// performs the HTTP/1.1 Upgrade handshake against its
// /sandbox/{id}/terminal endpoint. We bypass net/http.Client because it
// hides the post-101 raw byte stream we need to tunnel.
func (h *Handlers) dialAgentTerminal(ctx context.Context, node *models.Node, sandboxID string, q url.Values) (net.Conn, error) {
	u, err := url.Parse(h.Pool.ClientFor(node).BaseURL())
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if _, _, splitErr := net.SplitHostPort(addr); splitErr != nil {
		addr += ":80"
	}
	dialer := &net.Dialer{Timeout: terminalDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Forward only the handshake hints the agent understands.
	fwd := url.Values{}
	for _, k := range []string{"cols", "rows", "command"} {
		if v := q.Get(k); v != "" {
			fwd.Set(k, v)
		}
	}
	path := "/sandbox/" + url.PathEscape(sandboxID) + "/terminal"
	if enc := fwd.Encode(); enc != "" {
		path += "?" + enc
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: vajra-terminal\r\n" +
		"Authorization: Bearer " + h.AgentSharedSecret + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, errors.New("agent did not upgrade: " + resp.Status)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn is a net.Conn whose Read drains a bufio.Reader first, so
// any bytes http.ReadResponse buffered past the 101 response head are
// tunnelled rather than dropped.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

// Read forwards to the buffered reader.
func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// bridgeTerminal shuttles bytes between the browser WebSocket and the
// agent CONNECT tunnel until either side closes or ctx is cancelled.
func bridgeTerminal(ctx context.Context, ws *wsConn, agent net.Conn) {
	var (
		writeMu sync.Mutex // serialises browser-side WS writes
		wg      sync.WaitGroup
	)
	wg.Add(2)
	// Browser → agent.
	go func() {
		defer wg.Done()
		for {
			frame, err := ws.readFrame()
			if err != nil {
				_ = agent.Close()
				return
			}
			switch frame.Opcode {
			case wsOpcodeBinary:
				if err := writeAgentFrame(agent, 0x00, frame.Payload); err != nil {
					return
				}
			case wsOpcodeText:
				if err := handleTerminalText(agent, frame.Payload); err != nil {
					return
				}
			case wsOpcodePing:
				writeMu.Lock()
				_ = ws.writePong(frame.Payload)
				writeMu.Unlock()
			case wsOpcodeClose:
				writeMu.Lock()
				_ = ws.writeClose(1000, "client closed")
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
				werr := ws.writeBinary(buf[:n])
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
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		_ = agent.Close()
		_ = ws.close()
		<-done
	case <-done:
	}
}

// handleTerminalText translates a browser text frame. A {"resize":[r,c]}
// JSON object becomes the guest's framed resize message; anything else
// is treated as keystroke bytes (xterm.js, by default, ships input on
// text frames), so a terminal still works even if the client never
// switches to binary frames.
func handleTerminalText(agent net.Conn, payload []byte) error {
	var msg struct {
		Resize *[2]int `json:"resize"`
	}
	if err := json.Unmarshal(payload, &msg); err == nil && msg.Resize != nil {
		rows, cols := msg.Resize[0], msg.Resize[1]
		if rows > 0 && cols > 0 && rows < 1<<16 && cols < 1<<16 {
			frame := make([]byte, 9)
			frame[0] = 0x01 // resize
			binary.BigEndian.PutUint32(frame[1:5], 4)
			binary.BigEndian.PutUint16(frame[5:7], uint16(rows))
			binary.BigEndian.PutUint16(frame[7:9], uint16(cols))
			_, err := agent.Write(frame)
			return err
		}
		return nil
	}
	return writeAgentFrame(agent, 0x00, payload)
}

// writeAgentFrame wraps payload in the [type][uint32 len][payload]
// envelope the guest terminal agent expects on its vsock stream.
func writeAgentFrame(agent net.Conn, frameType byte, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = frameType
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
