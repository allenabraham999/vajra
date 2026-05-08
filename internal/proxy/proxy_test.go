package proxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSubdomain(t *testing.T) {
	cases := []struct {
		host, base string
		wantPort   int
		wantID     string
		wantErr    bool
	}{
		{"8080-abc123.vajra.dev", "vajra.dev", 8080, "abc123", false},
		{"3000-sb-deadbeef.example.com", "example.com", 3000, "sb-deadbeef", false},
		{"abc123.vajra.dev", "vajra.dev", 0, "", true},
		{"99999-abc.vajra.dev", "vajra.dev", 0, "", true},
		{"foo.bar.dev", "vajra.dev", 0, "", true},
		{"-abc.vajra.dev", "vajra.dev", 0, "", true},
		{"8080-.vajra.dev", "vajra.dev", 0, "", true},
	}
	for _, tc := range cases {
		port, id, err := parseSubdomain(tc.host, tc.base)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error", tc.host)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.host, err)
			continue
		}
		if port != tc.wantPort || id != tc.wantID {
			t.Errorf("%s: got (%d, %q), want (%d, %q)",
				tc.host, port, id, tc.wantPort, tc.wantID)
		}
	}
}

// TestForwardRoutesToAgent stands up a fake agent: a raw TCP listener
// that consumes the CONNECT-style upgrade request, writes 101, then
// answers a single HTTP request as if it were the user's app inside
// the sandbox. We confirm the proxy rewrites the upstream URL, sends
// the expected Upgrade and Auth headers, and copies the response back
// to the client.
func TestForwardRoutesToAgent(t *testing.T) {
	connectPath := make(chan string, 1)
	connectAuth := make(chan string, 1)
	upstream := startFakeAgent(t, func(t *testing.T, br *bufio.Reader, conn net.Conn) {
		req, err := http.ReadRequest(br)
		if err != nil {
			t.Errorf("read connect: %v", err)
			return
		}
		connectPath <- req.URL.Path
		connectAuth <- req.Header.Get("Authorization")
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: vajra-tcp\r\n" +
			"Connection: Upgrade\r\n\r\n"))
		// Now play HTTP server: read the actual user request, answer.
		userReq, err := http.ReadRequest(br)
		if err != nil {
			t.Errorf("read user req: %v", err)
			return
		}
		_ = userReq.Body.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n" +
			"Content-Length: 5\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"hello"))
	})
	defer upstream.Close()

	resolver := NewStaticResolver()
	resolver.Set("sb-1", SandboxRoute{
		SandboxID:    "sb-1",
		AgentBaseURL: "http://" + upstream.Addr().String(),
		AgentSecret:  "shhh",
		State:        "RUNNING",
	})
	srv, err := NewServer(Config{
		BaseDomain: "vajra.dev",
		Resolver:   resolver,
		Logger:     slog.Default(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://9000-sb-1.vajra.dev/healthz", nil)
	req.Host = "9000-sb-1.vajra.dev"
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello" {
		t.Errorf("body = %q", got)
	}
	if got := <-connectPath; got != "/sandbox/sb-1/forward/9000" {
		t.Errorf("agent path = %q", got)
	}
	if got := <-connectAuth; got != "Bearer shhh" {
		t.Errorf("auth = %q", got)
	}
}

// startFakeAgent stands up a single-connection raw TCP server. The
// handler is invoked with the bufio.Reader holding any pre-buffered
// request bytes plus the underlying conn.
func startFakeAgent(t *testing.T, handle func(t *testing.T, br *bufio.Reader, conn net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handle(t, bufio.NewReader(conn), conn)
	}()
	return ln
}

// TestForwardSandboxNotFound returns 404 from the resolver.
func TestForwardSandboxNotFound(t *testing.T) {
	resolver := NewStaticResolver() // empty
	srv, _ := NewServer(Config{BaseDomain: "vajra.dev", Resolver: resolver, Logger: slog.Default()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://80-missing.vajra.dev/", nil)
	req.Host = "80-missing.vajra.dev"
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestForwardNonRunningRejected confirms STOPPED/PENDING sandboxes 503.
func TestForwardNonRunningRejected(t *testing.T) {
	resolver := NewStaticResolver()
	resolver.Set("sb-1", SandboxRoute{SandboxID: "sb-1", State: "STOPPED"})
	srv, _ := NewServer(Config{BaseDomain: "vajra.dev", Resolver: resolver, Logger: slog.Default()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://80-sb-1.vajra.dev/", nil)
	req.Host = "80-sb-1.vajra.dev"
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestShareTokenRejectionBlocksRequest plumbs in a ShareValidator that
// always rejects; the handler must 403 before reaching the agent.
func TestShareTokenRejectionBlocksRequest(t *testing.T) {
	resolver := NewStaticResolver()
	resolver.Set("sb-1", SandboxRoute{SandboxID: "sb-1", AgentBaseURL: "http://unused", State: "RUNNING"})
	srv, _ := NewServer(Config{
		BaseDomain: "vajra.dev",
		Resolver:   resolver,
		Shares:     denyValidator{},
		Logger:     slog.Default(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://80-sb-1.vajra.dev/?token=bad", nil)
	req.Host = "80-sb-1.vajra.dev"
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

type denyValidator struct{}

func (denyValidator) ValidateShare(context.Context, string, string, int) error {
	return io.EOF
}

// TestStripPort exercises both Host header shapes.
func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"foo.bar":     "foo.bar",
		"foo.bar:443": "foo.bar",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	// IPv6 literals stay intact.
	got := stripPort("[2001:db8::1]:80")
	if !strings.HasPrefix(got, "[2001") {
		t.Errorf("ipv6 stripped wrongly: %q", got)
	}
}
