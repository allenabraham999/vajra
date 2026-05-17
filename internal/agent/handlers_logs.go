package agent

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// guestConsoleFile is the filename, under a sandbox's state directory,
// that the microVM's serial console is captured to when console capture
// is enabled. It is read best-effort: cloud-hypervisor is currently
// launched with the console disabled on the snapshot-restore hot path
// (see vmm.startProcess), so this file is usually absent and the guest
// source stays empty until console capture is wired in.
const guestConsoleFile = "console.log"

// maxGuestConsoleBytes caps how much of the console file is read so a
// runaway guest cannot make the logs endpoint allocate without bound.
const maxGuestConsoleBytes = 256 * 1024

// handleLogs serves GET /sandbox/{id}/logs?tail=N. It returns the
// agent-side ring-buffer tail for the sandbox plus, best-effort, any
// captured guest console output, merged and ordered oldest-first.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tail := 500
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = n
		}
	}

	entries := []LogEntry{}
	if s.logs != nil {
		entries = append(entries, s.logs.Tail(id, tail)...)
	}
	if sb, err := s.sandboxes.Get(id); err == nil && sb.StateDir != "" {
		entries = append(entries, readGuestConsole(sb.StateDir)...)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})
	if tail > 0 && len(entries) > tail {
		entries = entries[len(entries)-tail:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// readGuestConsole reads the captured serial console for a sandbox, if
// present, and returns one LogEntry per non-empty line. A missing file
// or any read error yields nil — guest console capture is best-effort.
func readGuestConsole(stateDir string) []LogEntry {
	path := filepath.Join(stateDir, guestConsoleFile)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	ts := time.Now().UTC()
	if st, statErr := f.Stat(); statErr == nil {
		ts = st.ModTime().UTC()
		// Only read the trailing window so a long-lived VM's console
		// does not blow the response up.
		if st.Size() > maxGuestConsoleBytes {
			_, _ = f.Seek(st.Size()-maxGuestConsoleBytes, 0)
		}
	}

	var out []LogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, LogEntry{
			Timestamp: ts,
			Source:    "guest",
			Level:     guestLineLevel(line),
			Message:   line,
		})
	}
	return out
}

// guestLineLevel makes a rough INFO/WARN/ERROR guess from a console line
// so kernel oopses and panics stand out in the viewer.
func guestLineLevel(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "panic"), strings.Contains(l, "error"),
		strings.Contains(l, "fail"), strings.Contains(l, "oops"):
		return "ERROR"
	case strings.Contains(l, "warn"):
		return "WARN"
	default:
		return "INFO"
	}
}
