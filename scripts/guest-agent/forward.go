//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// forwardRequest is the handshake the host writes before the connection
// flips into raw bidirectional bytes.
type forwardRequest struct {
	Port int    `json:"port"`           // localhost TCP port to forward to
	Host string `json:"host,omitempty"` // optional; defaults to 127.0.0.1
}

// serveForward bridges an incoming vsock connection to a TCP service
// running inside the guest. After parsing the JSON handshake and
// confirming with "ok\n", every byte sent over vsock is forwarded to
// localhost:port and vice versa.
func serveForward(c net.Conn, l *prefixLogger) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		l.Printf("read handshake: %v", err)
		return
	}
	var req forwardRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_, _ = fmt.Fprintf(c, "error: %s\n", err.Error())
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		_, _ = fmt.Fprintf(c, "error: invalid port %d\n", req.Port)
		return
	}
	host := req.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(req.Port))

	// Short connect timeout — if the user's app isn't up we want a fast
	// 502 on the proxy side, not a 30-second wait.
	target, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		_, _ = fmt.Fprintf(c, "error: dial %s: %s\n", addr, err.Error())
		return
	}
	defer target.Close()

	if _, err := c.Write([]byte("ok\n")); err != nil {
		l.Printf("write ok: %v", err)
		return
	}
	bridge(c, br, target, l)
}

// bridge fans bytes both ways between vsock and the target TCP socket.
// We use the buffered reader as the vsock-side source so any bytes that
// were already pulled in by the handshake reader are not lost.
func bridge(c net.Conn, br io.Reader, target net.Conn, l *prefixLogger) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(target, br)
		if err != nil {
			l.Printf("vsock→tcp: %v", err)
		}
		// Half-close the target so the user's app sees EOF after the
		// client finishes sending. CloseWrite isn't on net.Conn, but
		// the *net.TCPConn type does support it.
		if tc, ok := target.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(c, target)
		if err != nil {
			l.Printf("tcp→vsock: %v", err)
		}
		// vsock connection: close the write half so the host sees EOF.
		if uc, ok := c.(interface{ CloseWrite() error }); ok {
			_ = uc.CloseWrite()
		}
	}()
	wg.Wait()
}
