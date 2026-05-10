// Package main is the entry point for the vajra CLI.
//
// The vajra CLI lets users create, manage, and inspect sandboxes against
// a vajra-master endpoint. Configuration (api_url, api_key, jwt) lives at
// ~/.vajra/config.json; --api-url and --api-key flags override it.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, errStyle("error: ")+err.Error())
		os.Exit(1)
	}
}
