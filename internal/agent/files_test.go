package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestFileUploadRoundTrip sends a small body through FileUpload and
// asserts the guest sees a header with the right size + the body bytes.
func TestFileUploadRoundTrip(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	mgr, _, cacheDir := newTestManager(t)
	mgr.dialer = &fixedDialer{conn: host}
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	gotBody := make(chan []byte, 1)
	gotHeader := make(chan fileWireRequest, 1)
	go func() {
		br := bufio.NewReader(guest)
		line, _ := br.ReadBytes('\n')
		var req fileWireRequest
		_ = json.Unmarshal(line, &req)
		gotHeader <- req
		body := make([]byte, req.Size)
		_, _ = io.ReadFull(br, body)
		gotBody <- body
		resp, _ := json.Marshal(fileWireResponse{OK: true})
		_, _ = guest.Write(append(resp, '\n'))
	}()

	body := []byte("hello world")
	err = mgr.FileUpload(context.Background(), sb.ID, FileUploadRequest{
		Path:    "/tmp/hi",
		Mode:    0o644,
		Size:    int64(len(body)),
		Body:    bytes.NewReader(body),
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	hdr := <-gotHeader
	if hdr.Op != "upload" || hdr.Path != "/tmp/hi" || hdr.Size != int64(len(body)) {
		t.Fatalf("unexpected header: %+v", hdr)
	}
	if got := <-gotBody; !bytes.Equal(got, body) {
		t.Fatalf("guest received %q, want %q", got, body)
	}
}

// TestFileDownloadRoundTrip parses the streaming response shape: header
// JSON line, then exactly Size bytes.
func TestFileDownloadRoundTrip(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	mgr, _, cacheDir := newTestManager(t)
	mgr.dialer = &fixedDialer{conn: host}
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	go func() {
		br := bufio.NewReader(guest)
		_, _ = br.ReadBytes('\n')
		hdr, _ := json.Marshal(fileWireResponse{Size: 5, Mode: 0o644})
		_, _ = guest.Write(append(hdr, '\n'))
		_, _ = guest.Write([]byte("hello"))
	}()

	res, err := mgr.FileDownload(context.Background(), sb.ID, "/tmp/foo", 2*time.Second)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
	if res.Mode != 0o644 || res.Size != 5 {
		t.Fatalf("metadata mismatch: %+v", res)
	}
}

// TestFileListRoundTrip exercises FileList → JSON entries.
func TestFileListRoundTrip(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	mgr, _, cacheDir := newTestManager(t)
	mgr.dialer = &fixedDialer{conn: host}
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, _ := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})

	go func() {
		br := bufio.NewReader(guest)
		_, _ = br.ReadBytes('\n')
		now := time.Now().UTC().Truncate(time.Second)
		resp := fileWireResponse{Entries: []FileEntry{{Name: "a", Size: 1, Mode: 0o644, ModTime: now}}}
		buf, _ := json.Marshal(resp)
		_, _ = guest.Write(append(buf, '\n'))
	}()

	entries, err := mgr.FileList(context.Background(), sb.ID, "/", time.Second)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

// TestForwardHandshake validates DialForward writes the expected JSON
// handshake and treats "ok\n" as success.
func TestForwardHandshake(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	mgr, _, cacheDir := newTestManager(t)
	mgr.dialer = &fixedDialer{conn: host}
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, _ := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})

	gotPort := make(chan int, 1)
	go func() {
		br := bufio.NewReader(guest)
		line, _ := br.ReadBytes('\n')
		var req forwardWireRequest
		_ = json.Unmarshal(line, &req)
		gotPort <- req.Port
		_, _ = guest.Write([]byte("ok\n"))
		_, _ = guest.Write([]byte("hello-from-app"))
	}()

	conn, err := mgr.DialForward(context.Background(), sb.ID, 8080, "")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer conn.Close()
	if got := <-gotPort; got != 8080 {
		t.Fatalf("guest saw port %d, want 8080", got)
	}
	buf := make([]byte, 32)
	n, _ := conn.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "hello-from-app") {
		t.Fatalf("upstream bytes lost: %q", buf[:n])
	}
}

// TestForwardHandshakeRejection ensures a non-"ok" reply propagates as
// an error.
func TestForwardHandshakeRejection(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	mgr, _, cacheDir := newTestManager(t)
	mgr.dialer = &fixedDialer{conn: host}
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, _ := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})

	go func() {
		br := bufio.NewReader(guest)
		_, _ = br.ReadBytes('\n')
		_, _ = guest.Write([]byte("error: connection refused\n"))
	}()
	_, err := mgr.DialForward(context.Background(), sb.ID, 8080, "")
	if err == nil {
		t.Fatalf("expected forward to fail")
	}
}
