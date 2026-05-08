//go:build !linux

// Package main — stub for non-Linux builds. The guest agent is
// Linux-only (vsock + PTY) but `go build ./...` is run on macOS during
// development; this stub keeps the package compilable cross-platform.
package main

import "fmt"

func main() {
	fmt.Println("guest-agent: Linux-only binary; build with GOOS=linux")
}
