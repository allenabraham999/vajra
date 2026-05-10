package main

import (
	"github.com/spf13/cobra"
)

// version is the CLI build version. Overridable at link time:
//
//	go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" ./cmd/vajra
var version = "0.1.0"

// globalFlags is the bag of flags every subcommand inherits via the
// root cobra.Command. They're parsed by cobra into this struct, then
// merged with on-disk config inside resolveClient().
type globalFlags struct {
	apiURL  string
	apiKey  string
	asJSON  bool
	noColor bool
}

// gFlags is module-level so subcommands can read it without plumbing
// pointers through every constructor. Cobra initialises it before any
// RunE fires.
var gFlags globalFlags

// newRootCmd wires the top-level cobra command tree. Every subcommand
// is registered here so `vajra --help` enumerates the full surface.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vajra",
		Short:         "vajra — AI sandbox cloud platform CLI",
		Long:          "vajra creates, manages, and inspects sandboxes against a vajra-master endpoint.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&gFlags.apiURL, "api-url", "", "vajra-master base URL (overrides config)")
	pf.StringVar(&gFlags.apiKey, "api-key", "", "API key (overrides config)")
	pf.BoolVar(&gFlags.asJSON, "json", false, "output JSON instead of tables")
	pf.BoolVar(&gFlags.noColor, "no-color", false, "disable coloured output")

	root.AddCommand(
		newVersionCmd(),
		newLoginCmd(),
		newRegisterCmd(),
		newSandboxCmd(),
		newSnapshotCmd(),
		newTemplateCmd(),
		newNodeCmd(),
		newFileCmd(),
		newAPIKeyCmd(),
	)
	return root
}

// newVersionCmd prints the CLI version. Kept as its own subcommand so
// `vajra version` works as a discoverable command, while
// `vajra --version` covers the conventional flag form.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		RunE: func(_ *cobra.Command, _ []string) error {
			out("vajra " + version)
			return nil
		},
	}
}
