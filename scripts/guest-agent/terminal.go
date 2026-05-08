//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// terminalRequest is the JSON line the host writes before flipping the
// connection into raw bytes.
type terminalRequest struct {
	Command string   `json:"command,omitempty"` // empty = /bin/bash
	Args    []string `json:"args,omitempty"`
	Cols    uint16   `json:"cols,omitempty"`
	Rows    uint16   `json:"rows,omitempty"`
	Env     []string `json:"env,omitempty"` // KEY=VALUE entries
}

// Frame types on the host→guest direction. The guest→host stream is
// always raw PTY output (no framing — the host just relays bytes to the
// browser as binary WebSocket frames).
//
// Framing: one type byte + 4-byte BE length + payload.
const (
	frameData   byte = 0x00 // payload is bytes to write to PTY stdin
	frameResize byte = 0x01 // payload is 4 bytes: rows BE, cols BE
)

// serveTerminal handles one PTY session. After the handshake, the guest
// spawns a shell on a fresh pseudo-terminal, copies output to the vsock
// connection, and parses framed input from the connection back into
// stdin / SIGWINCH.
func serveTerminal(c net.Conn, l *prefixLogger) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		l.Printf("read handshake: %v", err)
		return
	}
	var req terminalRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_, _ = fmt.Fprintf(c, "error: %s\n", err)
		return
	}
	cmdName := req.Command
	if cmdName == "" {
		cmdName = "/bin/bash"
	}

	master, slaveName, err := openPTY()
	if err != nil {
		_, _ = fmt.Fprintf(c, "error: pty: %s\n", err)
		return
	}
	defer master.Close()

	if req.Cols > 0 && req.Rows > 0 {
		_ = setWinsize(master, req.Rows, req.Cols)
	}

	cmd := exec.Command(cmdName, req.Args...)
	cmd.Env = append(os.Environ(), append([]string{"TERM=xterm-256color"}, req.Env...)...)

	slave, err := os.OpenFile(slaveName, os.O_RDWR, 0)
	if err != nil {
		_, _ = fmt.Fprintf(c, "error: open slave: %s\n", err)
		return
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: int(slave.Fd())}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_, _ = fmt.Fprintf(c, "error: start: %s\n", err)
		return
	}
	// Parent doesn't need the slave end after fork; the child holds a dup.
	_ = slave.Close()

	if _, err := c.Write([]byte("ok\n")); err != nil {
		l.Printf("write ok: %v", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// PTY → vsock: forward raw output bytes verbatim.
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if _, werr := c.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		readFrames(br, master, l)
		// Reading EOF from the host means the WS was closed.
		// Kill the shell so the wait below returns.
		_ = cmd.Process.Signal(syscall.SIGHUP)
	}()
	_ = cmd.Wait()
	_ = master.Close()
	wg.Wait()
}

// readFrames parses the framed host→guest stream and dispatches data
// frames to the PTY master and resize frames to TIOCSWINSZ.
func readFrames(br *bufio.Reader, master *os.File, l *prefixLogger) {
	for {
		typeByte, err := br.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				l.Printf("read type: %v", err)
			}
			return
		}
		var lenBuf [4]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			l.Printf("read length: %v", err)
			return
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		// Cap to 64 KiB so a malicious / buggy host can't make us
		// allocate a multi-GB buffer for one frame.
		if n > 64*1024 {
			l.Printf("frame too large: %d", n)
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			l.Printf("read payload: %v", err)
			return
		}
		switch typeByte {
		case frameData:
			if _, err := master.Write(payload); err != nil {
				l.Printf("pty write: %v", err)
				return
			}
		case frameResize:
			if len(payload) != 4 {
				continue
			}
			rows := binary.BigEndian.Uint16(payload[0:2])
			cols := binary.BigEndian.Uint16(payload[2:4])
			_ = setWinsize(master, rows, cols)
		default:
			// Unknown frame type — drop so future versions can extend.
		}
	}
}

// openPTY allocates a pseudo-terminal pair and returns the master end
// plus the path of the slave PTY (e.g. /dev/pts/3).
func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open ptmx: %w", err)
	}
	// Unlock the slave (default state on /dev/ptmx is locked).
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, "", fmt.Errorf("unlockpt: %w", err)
	}
	pts, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, "", fmt.Errorf("ptsname: %w", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", pts), nil
}

// setWinsize is a TIOCSWINSZ on master so child processes see the new
// size and the kernel sends them SIGWINCH.
func setWinsize(master *os.File, rows, cols uint16) error {
	ws := &unix.Winsize{Row: rows, Col: cols}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws)
}
