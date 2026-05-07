// Package main is the entry point for the vajra CLI.
//
// The vajra CLI lets users create, manage, and inspect sandboxes against
// a vajra-master endpoint.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("vajra — AI sandbox cloud platform")
		fmt.Println("usage: vajra <command> [args]")
		oss.Exit(0)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("vajra 0.0.1")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
