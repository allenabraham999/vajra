package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/fatih/color"
)

// out prints a line to stdout. A function so we can swap it in tests.
func out(s string) {
	fmt.Fprintln(os.Stdout, s)
}

// printJSON marshals v with indentation and writes it to stdout. Used
// by every command when --json is set.
func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(b))
	return nil
}

// errStyle wraps a string in red ANSI codes for stderr error printing.
// fatih/color honours the NO_COLOR env var and TTY detection on its own.
func errStyle(s string) string {
	if gFlags.noColor {
		return s
	}
	return color.RedString("%s", s)
}

// table renders rows as a tab-aligned table to stdout. headers length
// must match each row's length; mismatched rows are skipped silently.
func table(headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	cols := len(headers)
	header := joinCells(headers)
	if !gFlags.noColor {
		header = color.New(color.Bold).Sprint(header)
	}
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		if len(r) != cols {
			continue
		}
		fmt.Fprintln(tw, joinCells(r))
	}
	_ = tw.Flush()
}

// joinCells joins cells with tab separators for tabwriter.
func joinCells(cells []string) string {
	out := ""
	for i, c := range cells {
		if i > 0 {
			out += "\t"
		}
		out += c
	}
	return out
}

// stateColor wraps a sandbox/node state in a colour tied to its meaning.
// Returns the raw string when colour is disabled.
func stateColor(state string) string {
	if gFlags.noColor {
		return state
	}
	switch state {
	case "RUNNING", "ACTIVE":
		return color.GreenString("%s", state)
	case "CREATING", "STARTING", "PAUSING", "STOPPING", "DESTROYING", "REGISTERING":
		return color.YellowString("%s", state)
	case "ERROR", "QUARANTINED":
		return color.RedString("%s", state)
	case "STOPPED", "DESTROYED", "OFFLINE", "ARCHIVED", "DRAINING":
		return color.HiBlackString("%s", state)
	default:
		return state
	}
}
