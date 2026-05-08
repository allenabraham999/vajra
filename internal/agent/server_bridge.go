// Package agent — server_bridge.go is the agent's HTTP→vsock bridge.
// vajra-proxy turns an inbound HTTPS / WebSocket request into a forward
// or terminal session by HTTP/1.1-Upgrading to one of these endpoints.
// We hijack the connection, run a tiny "ok"/"error" handshake, then
// shovel bytes between the wire and a vsock session into the guest
// agent.
package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// bridgeUpgradeProto is the value vajra-proxy sends in `Upgrade:` so the
// agent knows which guest port to wire the connection to. Two values:
// "vajra-tcp" (forward to a localhost TCP port inside the VM) and
// "vajra-terminal" (PTY session). Anything else is rejected before the
// hijack.
const bridgeUpgradeProto = "vajra-tcp"

// handleForward is the agent endpoint vajra-proxy targets to open a
// stream into a TCP service running inside a sandbox VM. The path is
// `/sandbox/{id}/forward/{port}`. We hijack the HTTP connection, write
// the 101 Switching Protocols handshake, then bridge bytes to the
// guest's vsock forward port.
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	portStr := r.PathValue("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		writeErr(w, http.StatusBadRequest, "invalid port")
		return
	}

	guest, err := s.sandboxes.DialForward(r.Context(), id, port, "")
	if err != nil {
		s.logger.Warn("forward: dial guest", "sandbox", id, "port", port, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer guest.Close()

	client, brw, err := hijackConn(w)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer client.Close()

	if err := writeUpgrade(client, "vajra-tcp"); err != nil {
		s.logger.Warn("forward: write upgrade", "err", err)
		return
	}
	bridgeBytes(client, brw, guest, s)
}

// handleTerminal hijacks an HTTP/1.1 connection and bridges the raw
// bytes to the guest terminal port. The proxy is the only reasonable
// caller — browsers come in via a WebSocket frame translator there.
//
// We accept the same handshake parameters the proxy collected from the
// client via query string (cols, rows, command).
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	cmd := q.Get("command")
	guest, err := s.sandboxes.DialTerminal(r.Context(), id, TerminalOpts{
		Command: cmd,
		Cols:    uint16(cols),
		Rows:    uint16(rows),
	})
	if err != nil {
		s.logger.Warn("terminal: dial guest", "sandbox", id, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer guest.Close()

	client, brw, err := hijackConn(w)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer client.Close()

	if err := writeUpgrade(client, "vajra-terminal"); err != nil {
		s.logger.Warn("terminal: write upgrade", "err", err)
		return
	}
	bridgeBytes(client, brw, guest, s)
}

// hijackConn wraps the standard "is the responsewriter hijackable" dance
// behind a single helper. Returns the raw TCP conn plus the bufio
// reader/writer that may already contain bytes from the request.
func hijackConn(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("hijack: %w", err)
	}
	return conn, brw, nil
}

// writeUpgrade emits the 101 Switching Protocols line + the per-stream
// Upgrade token. The trailing CRLF terminates the response head.
func writeUpgrade(c net.Conn, proto string) error {
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: " + proto + "\r\n\r\n"
	_, err := c.Write([]byte(resp))
	return err
}

// bridgeBytes copies bytes both ways between the hijacked client conn
// and the guest vsock conn. Any leftover buffered bytes from the
// request reader are flushed into the guest first so they aren't lost.
func bridgeBytes(client net.Conn, brw *bufio.ReadWriter, guest io.ReadWriteCloser, s *Server) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Drain any pre-buffered bytes from the bufio.Reader, then
		// switch to direct copy off the wire.
		if brw != nil && brw.Reader.Buffered() > 0 {
			if _, err := io.CopyN(guest, brw.Reader, int64(brw.Reader.Buffered())); err != nil {
				return
			}
		}
		if _, err := io.Copy(guest, client); err != nil {
			s.logger.Debug("bridge client→guest", "err", err)
		}
		if cw, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(client, guest); err != nil {
			s.logger.Debug("bridge guest→client", "err", err)
		}
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}

// _ = bridgeUpgradeProto keeps the const exported-comment lint happy
// while the value is referenced only inside the package.
var _ = bridgeUpgradeProto
