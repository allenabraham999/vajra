//go:build linux

// guest-agent is the in-VM helper Vajra installs in every sandbox rootfs.
// It listens on a handful of vsock ports and serves the host-side request
// shapes defined in internal/agent — exec, files, port-forward, terminal.
//
// Build: CGO_ENABLED=0 GOOS=linux go build -o guest-agent ./scripts/guest-agent
//
// The binary is intentionally tiny — no third-party deps, just stdlib +
// golang.org/x/sys/unix — so it can be stripped, dropped into an
// ext4 rootfs, and started by /sbin/init or a one-liner systemd unit.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Port assignments. Mirrors internal/agent (host-side) constants — keep
// them in sync. Each port is a single-purpose service so a newly opened
// connection unambiguously selects an op.
const (
	PortExec     uint32 = 5252 // exec service (untyped JSON)
	PortFiles    uint32 = 5253 // file ops (typed JSON)
	PortTerminal uint32 = 5254 // PTY terminal (handshake + framed bytes)
	PortForward  uint32 = 5255 // localhost TCP forward (handshake + raw)
)

func main() {
	var (
		execPort     = flag.Int("exec-port", int(PortExec), "vsock port for exec service")
		filesPort    = flag.Int("files-port", int(PortFiles), "vsock port for file service")
		terminalPort = flag.Int("terminal-port", int(PortTerminal), "vsock port for terminal service")
		forwardPort  = flag.Int("forward-port", int(PortForward), "vsock port for tcp-forward service")
	)
	flag.Parse()

	base := log.New(os.Stderr, "guest-agent ", log.LstdFlags)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Each service runs on its own goroutine and bails the whole process
	// out if it can't bind. A failed bind almost always means the kernel
	// doesn't have AF_VSOCK or another guest-agent is already running —
	// either way, refusing to start is the right call.
	var wg sync.WaitGroup
	type svc struct {
		name    string
		port    uint32
		handler func(net.Conn, *prefixLogger)
	}
	for _, s := range []svc{
		{"exec", uint32(*execPort), serveExec},
		{"files", uint32(*filesPort), serveFiles},
		{"terminal", uint32(*terminalPort), serveTerminal},
		{"forward", uint32(*forwardPort), serveForward},
	} {
		wg.Add(1)
		l := newPrefixLogger(base, s.name)
		go func(name string, port uint32, l *prefixLogger, h func(net.Conn, *prefixLogger)) {
			defer wg.Done()
			if err := serveVsock(ctx, port, h, l); err != nil {
				l.Printf("listen: %v", err)
				cancel()
			}
		}(s.name, s.port, l, s.handler)
	}
	base.Printf("ready on vsock ports %d/%d/%d/%d",
		*execPort, *filesPort, *terminalPort, *forwardPort)
	wg.Wait()
	base.Printf("shutting down")
}

// prefixLogger is a tiny wrapper that prefixes every log line with the
// service name. Avoids pulling in slog (smaller static binary).
type prefixLogger struct {
	base   *log.Logger
	prefix string
}

func newPrefixLogger(base *log.Logger, prefix string) *prefixLogger {
	return &prefixLogger{base: base, prefix: prefix}
}

// Printf logs a formatted message with the configured prefix.
func (l *prefixLogger) Printf(format string, args ...any) {
	l.base.Printf("["+l.prefix+"] "+format, args...)
}
