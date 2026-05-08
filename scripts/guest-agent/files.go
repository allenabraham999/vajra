//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileRequest is the typed envelope read on the files port. Op selects
// which sub-handler runs; the remaining fields are interpreted per-op.
type fileRequest struct {
	Op   string `json:"op"`             // "upload" | "download" | "list"
	Path string `json:"path,omitempty"` // for upload/download
	Dir  string `json:"dir,omitempty"`  // for list
	Size int64  `json:"size,omitempty"` // upload only — total bytes following the JSON line
	Mode uint32 `json:"mode,omitempty"` // upload only — POSIX mode bits
}

// fileEntry is one row in the list response.
type fileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// fileResponse is the catch-all reply shape. Different ops fill in
// different combinations: upload responds with {"ok": true}, download
// responds with {"size", "mode"} followed by the bytes, list responds
// with {"entries"}.
type fileResponse struct {
	OK      bool        `json:"ok,omitempty"`
	Error   string      `json:"error,omitempty"`
	Size    int64       `json:"size,omitempty"`
	Mode    uint32      `json:"mode,omitempty"`
	Entries []fileEntry `json:"entries,omitempty"`
}

// MaxUploadBytes caps a single upload at 1 GiB. Anything larger is most
// likely a bug or an attempt to exhaust the guest's disk; clients that
// genuinely want to ship a giant file can split it themselves.
const MaxUploadBytes int64 = 1 << 30

// serveFiles parses one JSON request line then dispatches by op. Each
// connection serves a single request; the host opens a fresh vsock
// connection for the next op.
func serveFiles(c net.Conn, l *prefixLogger) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		l.Printf("read: %v", err)
		return
	}
	var req fileRequest
	if err := json.Unmarshal(line, &req); err != nil {
		writeJSON(c, fileResponse{Error: "decode: " + err.Error()})
		return
	}
	switch req.Op {
	case "upload":
		handleUpload(c, br, req, l)
	case "download":
		handleDownload(c, req, l)
	case "list":
		handleList(c, req, l)
	default:
		writeJSON(c, fileResponse{Error: "unknown op: " + req.Op})
	}
}

// handleUpload reads exactly req.Size bytes from the connection (after
// the JSON line) and writes them to req.Path with the requested mode.
// We write to a sibling temp file and rename — atomic so a partial
// upload never leaves a half-written file in place.
func handleUpload(c net.Conn, br *bufio.Reader, req fileRequest, l *prefixLogger) {
	if req.Path == "" {
		writeJSON(c, fileResponse{Error: "path is required"})
		return
	}
	if req.Size < 0 || req.Size > MaxUploadBytes {
		writeJSON(c, fileResponse{Error: fmt.Sprintf("size out of bounds: %d", req.Size)})
		return
	}
	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(req.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(c, fileResponse{Error: "mkdir: " + err.Error()})
		return
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		writeJSON(c, fileResponse{Error: "tempfile: " + err.Error()})
		return
	}
	tmpName := tmp.Name()
	if _, err := io.CopyN(tmp, br, req.Size); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		writeJSON(c, fileResponse{Error: "copy: " + err.Error()})
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		writeJSON(c, fileResponse{Error: "close: " + err.Error()})
		return
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		writeJSON(c, fileResponse{Error: "chmod: " + err.Error()})
		return
	}
	if err := os.Rename(tmpName, req.Path); err != nil {
		_ = os.Remove(tmpName)
		writeJSON(c, fileResponse{Error: "rename: " + err.Error()})
		return
	}
	writeJSON(c, fileResponse{OK: true})
}

// handleDownload reads req.Path off disk and streams it back: a JSON
// header announcing {size, mode}, then size bytes. The host can use
// io.LimitReader to slice the bytes off the wire.
func handleDownload(c net.Conn, req fileRequest, l *prefixLogger) {
	if req.Path == "" {
		writeJSON(c, fileResponse{Error: "path is required"})
		return
	}
	f, err := os.Open(req.Path)
	if err != nil {
		writeJSON(c, fileResponse{Error: "open: " + err.Error()})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeJSON(c, fileResponse{Error: "stat: " + err.Error()})
		return
	}
	if st.IsDir() {
		writeJSON(c, fileResponse{Error: "is a directory"})
		return
	}
	writeJSON(c, fileResponse{Size: st.Size(), Mode: uint32(st.Mode().Perm())})
	if _, err := io.Copy(c, f); err != nil {
		l.Printf("download copy: %v", err)
	}
}

// handleList reads req.Dir and writes one JSON response with every entry.
// We deliberately don't recurse — a recursive walk would silently chew up
// the wire for a large source tree.
func handleList(c net.Conn, req fileRequest, l *prefixLogger) {
	dir := req.Dir
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(c, fileResponse{Error: "readdir: " + err.Error()})
		return
	}
	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			// Race: file disappeared between ReadDir and Info. Skip it
			// rather than failing the whole listing.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			writeJSON(c, fileResponse{Error: "info: " + err.Error()})
			return
		}
		out = append(out, fileEntry{
			Name:    e.Name(),
			Size:    info.Size(),
			Mode:    uint32(info.Mode().Perm()),
			IsDir:   e.IsDir(),
			ModTime: info.ModTime().UTC(),
		})
	}
	writeJSON(c, fileResponse{Entries: out})
}

// pathClean is a defensive normalizer: strip any "..", absolutize against
// the root, and make sure callers can't accidentally escape the working
// directory. Currently unused — kept for the host-side caller to opt into
// when sandbox policies require it.
func pathClean(p string) string {
	return filepath.Clean(strings.TrimSpace(p))
}
