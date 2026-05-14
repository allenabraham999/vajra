package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// sandboxConfig mirrors models.SandboxConfig.
type sandboxConfig struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
	DiskGB   int `json:"disk_gb"`
}

// sandbox mirrors models.Sandbox plus the optional operation_id master
// returns on lifecycle endpoints.
type sandbox struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	AccountID   string        `json:"account_id"`
	NodeID      *string       `json:"node_id,omitempty"`
	ClusterID   *string       `json:"cluster_id,omitempty"`
	TemplateID  string        `json:"template_id"`
	State       string        `json:"state"`
	Config      sandboxConfig `json:"config"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	OperationID string        `json:"operation_id,omitempty"`
}

// execResult mirrors master ExecResult.
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// newSandboxCmd builds the `vajra sandbox` subtree.
func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Short:   "Manage sandboxes",
		Aliases: []string{"sb"},
	}
	cmd.AddCommand(
		newSandboxCreateCmd(),
		newSandboxListCmd(),
		newSandboxGetCmd(),
		newSandboxExecCmd(),
		newSandboxStopCmd(),
		newSandboxStartCmd(),
		newSandboxDestroyCmd(),
		newSandboxArchiveCmd(),
		newSandboxRehydrateCmd(),
		newSandboxMigrateCmd(),
	)
	return cmd
}

// archiveResult is the body returned by POST /v1/sandboxes/{id}/archive.
type archiveResult struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	Path        string `json:"path"`
	Location    string `json:"location"`
	SizeBytes   int64  `json:"size_bytes"`
}

// migrateResult is the body returned by POST /v1/sandboxes/{id}/migrate.
type migrateResult struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	SourceNode  string `json:"source_node_id"`
	TargetNode  string `json:"target_node_id"`
	BytesSent   int64  `json:"bytes_sent"`
}

func newSandboxArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <id>",
		Short: "Stop and compress a sandbox into cold storage",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var res archiveResult
			if err := c.do(ctx, "POST", "/v1/sandboxes/"+args[0]+"/archive", nil, &res); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(res)
			}
			table([]string{"FIELD", "VALUE"}, [][]string{
				{"ID", res.ID},
				{"Operation", res.OperationID},
				{"Location", res.Location},
				{"Path", res.Path},
				{"Size", fmt.Sprintf("%d bytes", res.SizeBytes)},
			})
			return nil
		},
	}
}

func newSandboxRehydrateCmd() *cobra.Command {
	var archivePath, nodeID string
	cmd := &cobra.Command{
		Use:   "rehydrate <id>",
		Short: "Restore an archived sandbox to STOPPED",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			body := map[string]any{}
			if archivePath != "" {
				body["archive_path"] = archivePath
			}
			if nodeID != "" {
				body["node_id"] = nodeID
			}
			var sb sandbox
			if err := c.do(ctx, "POST", "/v1/sandboxes/"+args[0]+"/rehydrate", body, &sb); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sb)
			}
			renderSandbox(&sb)
			return nil
		},
	}
	cmd.Flags().StringVar(&archivePath, "archive-path", "", "explicit archive locator (path or s3://...)")
	cmd.Flags().StringVar(&nodeID, "node", "", "target node ID (default: original node or scheduler)")
	return cmd
}

func newSandboxMigrateCmd() *cobra.Command {
	var targetNode string
	cmd := &cobra.Command{
		Use:   "migrate <id> --target <node-id>",
		Short: "Move a sandbox to another node (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if targetNode == "" {
				return fmt.Errorf("--target is required")
			}
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			body := map[string]any{"target_node_id": targetNode}
			var res migrateResult
			if err := c.do(ctx, "POST", "/v1/sandboxes/"+args[0]+"/migrate", body, &res); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(res)
			}
			table([]string{"FIELD", "VALUE"}, [][]string{
				{"ID", res.ID},
				{"Operation", res.OperationID},
				{"From node", res.SourceNode},
				{"To node", res.TargetNode},
				{"Bytes sent", fmt.Sprintf("%d", res.BytesSent)},
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&targetNode, "target", "", "target node ID (required)")
	return cmd
}

func newSandboxCreateCmd() *cobra.Command {
	var (
		name, template, snapshot, region string
		vcpus, memoryMB, diskGB          int
		autoStop, autoArchive            int
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new sandbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if template == "" && snapshot == "" {
				return fmt.Errorf("--template or --snapshot is required")
			}
			source := "image"
			body := map[string]any{
				"name":      name,
				"vcpus":     vcpus,
				"memory_mb": memoryMB,
				"disk_gb":   diskGB,
			}
			if snapshot != "" {
				source = "snapshot"
				body["snapshot_id"] = snapshot
			} else {
				body["template_id"] = template
			}
			body["source"] = source
			if region != "" {
				body["region"] = region
			}
			if cmd.Flags().Changed("auto-stop") {
				body["auto_stop_minutes"] = autoStop
			}
			if cmd.Flags().Changed("auto-archive") {
				body["auto_archive_minutes"] = autoArchive
			}

			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var sb sandbox
			if err := c.do(ctx, "POST", "/v1/sandboxes", body, &sb); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sb)
			}
			renderSandbox(&sb)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "sandbox name")
	cmd.Flags().StringVar(&template, "template", "", "template ID (image source)")
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "snapshot ID (snapshot source)")
	cmd.Flags().IntVar(&vcpus, "vcpu", 2, "vCPU count")
	cmd.Flags().IntVar(&memoryMB, "memory", 512, "memory in MB")
	cmd.Flags().IntVar(&diskGB, "disk", 5, "disk in GB")
	cmd.Flags().StringVar(&region, "region", "", "region (optional)")
	cmd.Flags().IntVar(&autoStop, "auto-stop", 15, "idle minutes before auto-stop (0 to disable)")
	cmd.Flags().IntVar(&autoArchive, "auto-archive", 1440, "idle minutes before auto-archive (0 to disable)")
	return cmd
}

func newSandboxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sandboxes",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var sbs []sandbox
			if err := c.do(ctx, "GET", "/v1/sandboxes", nil, &sbs); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sbs)
			}
			rows := make([][]string, 0, len(sbs))
			for _, s := range sbs {
				node := ""
				if s.NodeID != nil {
					node = *s.NodeID
				}
				rows = append(rows, []string{
					s.ID, s.Name, stateColor(s.State), s.TemplateID,
					node, s.CreatedAt.Format(time.RFC3339),
				})
			}
			table([]string{"ID", "NAME", "STATE", "TEMPLATE", "NODE", "CREATED"}, rows)
			return nil
		},
	}
}

func newSandboxGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show one sandbox by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var sb sandbox
			if err := c.do(ctx, "GET", "/v1/sandboxes/"+args[0], nil, &sb); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sb)
			}
			renderSandbox(&sb)
			return nil
		},
	}
}

func newSandboxExecCmd() *cobra.Command {
	var timeoutMS int64
	cmd := &cobra.Command{
		Use:   "exec <id> <command>",
		Short: "Run a command inside a sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			body := map[string]any{"command": args[1]}
			if timeoutMS > 0 {
				body["timeout_ms"] = timeoutMS
			}
			var res execResult
			if err := c.do(ctx, "POST", "/v1/sandboxes/"+args[0]+"/exec", body, &res); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(res)
			}
			if res.Stdout != "" {
				fmt.Print(res.Stdout)
			}
			if res.Stderr != "" {
				fmt.Print(errStyle(res.Stderr))
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("command exited %d", res.ExitCode)
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&timeoutMS, "timeout-ms", 0, "command timeout in ms (0 = server default)")
	return cmd
}

func newSandboxStopCmd() *cobra.Command {
	return lifecycleCmd("stop", "Stop a running sandbox", "/stop")
}

func newSandboxStartCmd() *cobra.Command {
	return lifecycleCmd("start", "Start a stopped sandbox", "/start")
}

func newSandboxDestroyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "destroy <id>",
		Short:   "Destroy a sandbox",
		Aliases: []string{"rm", "delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var sb sandbox
			if err := c.do(ctx, "DELETE", "/v1/sandboxes/"+args[0], nil, &sb); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sb)
			}
			out("sandbox " + args[0] + " — " + stateColor(sb.State))
			return nil
		},
	}
	return cmd
}

// lifecycleCmd builds the start/stop sandbox sub-commands. Both POST
// against /v1/sandboxes/{id}<suffix> and re-render the returned row.
func lifecycleCmd(name, short, suffix string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var sb sandbox
			if err := c.do(ctx, "POST", "/v1/sandboxes/"+args[0]+suffix, nil, &sb); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sb)
			}
			out("sandbox " + sb.ID + " — " + stateColor(sb.State))
			return nil
		},
	}
}

// renderSandbox prints a key/value-style block for a single sandbox.
func renderSandbox(s *sandbox) {
	node := ""
	if s.NodeID != nil {
		node = *s.NodeID
	}
	rows := [][]string{
		{"ID", s.ID},
		{"Name", s.Name},
		{"State", stateColor(s.State)},
		{"Template", s.TemplateID},
		{"Node", node},
		{"vCPU", fmt.Sprintf("%d", s.Config.VCPUs)},
		{"Memory MB", fmt.Sprintf("%d", s.Config.MemoryMB)},
		{"Disk GB", fmt.Sprintf("%d", s.Config.DiskGB)},
		{"Created", s.CreatedAt.Format(time.RFC3339)},
		{"Updated", s.UpdatedAt.Format(time.RFC3339)},
	}
	if s.OperationID != "" {
		rows = append(rows, []string{"Operation", s.OperationID})
	}
	table([]string{"FIELD", "VALUE"}, rows)
}
