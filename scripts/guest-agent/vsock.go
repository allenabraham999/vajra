//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"

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

// vsockConn wraps an accepted vsock fd into a net.Conn. We go via
// os.NewFile + net.FileConn so the runtime takes ownership of close,
// timeouts (where supported), and goroutine integration.
func vsockConn(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "vsock")
	if f == nil {
		return nil, fmt.Errorf("os.NewFile returned nil for fd %d", fd)
	}
	defer f.Close() // FileConn dups; the original wrapper is no longer needed.
	return net.FileConn(f)
}
