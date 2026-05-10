//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// serveVsock binds a SOCK_STREAM vsock socket on (CID_ANY, port), accepts
// connections, and spawns handler for each. Cancellation of ctx triggers
// a clean shutdown — any in-flight handlers continue until they return on
// their own (typical short-lived RPCs).
func serveVsock(ctx context.Context, port uint32, handler func(net.Conn, *prefixLogger), l *prefixLogger) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	addr := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, addr); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("listen: %w", err)
	}

	// FileListener wraps the kernel fd so we can use net.Conn semantics
	// for accepted sockets. The Listener doesn't itself satisfy
	// net.Listener for AF_VSOCK, but FileConn on each accept does.
	go func() {
		<-ctx.Done()
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		_ = unix.Close(fd)
	}()

	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// EAGAIN/EINTR shouldn't happen on a blocking socket, but
			// stay defensive — log and retry.
			l.Printf("accept: %v", err)
			continue
		}
		conn, err := vsockConn(nfd)
		if err != nil {
			_ = unix.Close(nfd)
			l.Printf("wrap: %v", err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			handler(c, l)
		}(conn)
	}
}

// vsockConn wraps an accepted vsock fd into a net.Conn. We can't use
// net.FileConn here: it calls getsockname() to recover the address family
// and Go's stdlib doesn't recognise AF_VSOCK, so it returns
// "address family not supported by protocol" and every accepted connection
// is dropped before the handler ever sees it. Wrapping os.File directly
// gives us Read/Write/Close + per-op deadlines via the runtime poller, and
// we provide stub addresses so callers that only need net.Conn semantics
// keep working.
func vsockConn(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "vsock")
	if f == nil {
		return nil, fmt.Errorf("os.NewFile returned nil for fd %d", fd)
	}
	return &vsockNetConn{f: f}, nil
}

// vsockNetConn adapts os.File to net.Conn for an AF_VSOCK fd.
type vsockNetConn struct {
	f *os.File
}

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

func (c *vsockNetConn) Read(p []byte) (int, error)         { return c.f.Read(p) }
func (c *vsockNetConn) Write(p []byte) (int, error)        { return c.f.Write(p) }
func (c *vsockNetConn) Close() error                       { return c.f.Close() }
func (c *vsockNetConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *vsockNetConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *vsockNetConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *vsockNetConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockNetConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
