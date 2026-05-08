package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func bufWriter(w io.Writer) *bufio.Writer { return bufio.NewWriter(w) }
func bufReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

func TestAcceptKey(t *testing.T) {
	// RFC 6455 §1.3 worked example: "dGhlIHNhbXBsZSBub25jZQ==" →
	// "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=".
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("acceptKey = %q", got)
	}
}

func TestHeaderContains(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Connection", "keep-alive, Upgrade")
	if !headerContains(r.Header, "Connection", "upgrade") {
		t.Errorf("expected case-insensitive match")
	}
	if headerContains(r.Header, "Connection", "close") {
		t.Errorf("unexpected match")
	}
}

// TestFrameRoundTrip writes a server-side frame, then parses it back as
// if we were the client. Verifies our writer doesn't mask and the
// reader handles unmasked frames.
func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	bw := bufWriter(&buf)
	if err := writeFrame(bw, OpcodeBinary, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufReader(&buf)
	frame, err := readFrame(br)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if frame.Opcode != OpcodeBinary || string(frame.Payload) != "hello" {
		t.Fatalf("unexpected frame: %+v", frame)
	}
}

// TestExtendedLengthRoundTrip exercises the 16-bit length path
// (payload len 200).
func TestExtendedLengthRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	bw := bufWriter(&buf)
	payload := bytes.Repeat([]byte{'x'}, 200)
	_ = writeFrame(bw, OpcodeBinary, payload)
	br := bufReader(&buf)
	frame, err := readFrame(br)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Fatalf("payload mismatch")
	}
}

// TestMaskedClientFrame ensures the reader strips the client mask.
func TestMaskedClientFrame(t *testing.T) {
	// Build a masked binary frame with payload "abc" by hand.
	frame := []byte{
		0x82,                   // FIN + binary
		0x80 | 3,               // mask + length 3
		0x01, 0x02, 0x03, 0x04, // mask key
	}
	mask := []byte{0x01, 0x02, 0x03, 0x04}
	for i, b := range []byte("abc") {
		frame = append(frame, b^mask[i%4])
	}
	br := bufReader(bytes.NewReader(frame))
	parsed, err := readFrame(br)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(parsed.Payload) != "abc" {
		t.Fatalf("payload = %q", parsed.Payload)
	}
}

func TestUpgradeRejectsWrongHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	if _, err := Upgrade(w, r); err == nil {
		t.Fatalf("expected upgrade rejection on missing headers")
	}
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Version", "8")
	r.Header.Set("Sec-WebSocket-Key", "abc")
	if _, err := Upgrade(w, r); err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Version") {
		t.Fatalf("expected version rejection, got %v", err)
	}
}
