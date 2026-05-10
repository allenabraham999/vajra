package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// snapshot mirrors models.Snapshot.
type snapshot struct {
	ID          string    `json:"id"`
	SandboxID   string    `json:"sandbox_id"`
	AccountID   string    `json:"account_id"`
	NodeID      string    `json:"node_id"`
	StoragePath string    `json:"storage_path"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

// newSnapshotCmd builds the `vajra snapshot` subtree.
func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage sandbox snapshots",
	}
	cmd.AddCommand(
		newSnapshotCreateCmd(),
		newSnapshotListCmd(),
		newSnapshotRestoreCmd(),
	)
	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create <sandbox-id>",
		Short: "Snapshot a sandbox's state",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
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
			var snap snapshot
			if err := c.do(ctx, "POST", "/v1/sandboxes/"+args[0]+"/snapshot",
				map[string]string{"name": name}, &snap); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(snap)
			}
			out(fmt.Sprintf("snapshot %s (%d bytes)", snap.ID, snap.SizeBytes))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "snapshot label")
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <sandbox-id>",
		Short: "List snapshots for a sandbox",
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
			var snaps []snapshot
			if err := c.do(ctx, "GET", "/v1/sandboxes/"+args[0]+"/snapshots", nil, &snaps); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(snaps)
			}
			rows := make([][]string, 0, len(snaps))
			for _, s := range snaps {
				rows = append(rows, []string{
					s.ID, s.SandboxID, s.NodeID,
					fmt.Sprintf("%d", s.SizeBytes),
					s.CreatedAt.Format(time.RFC3339),
				})
			}
			table([]string{"ID", "SANDBOX", "NODE", "SIZE", "CREATED"}, rows)
			return nil
		},
	}
}

func newSnapshotRestoreCmd() *cobra.Command {
	var (
		name                    string
		vcpus, memoryMB, diskGB int
	)
	cmd := &cobra.Command{
		Use:   "restore <snapshot-id>",
		Short: "Create a new sandbox from a snapshot",
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
			body := map[string]any{
				"name":      name,
				"vcpus":     vcpus,
				"memory_mb": memoryMB,
				"disk_gb":   diskGB,
			}
			var sb sandbox
			if err := c.do(ctx, "POST", "/v1/snapshots/"+args[0]+"/restore", body, &sb); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(sb)
			}
			renderSandbox(&sb)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name for the restored sandbox")
	cmd.Flags().IntVar(&vcpus, "vcpu", 2, "vCPU count")
	cmd.Flags().IntVar(&memoryMB, "memory", 512, "memory in MB")
	cmd.Flags().IntVar(&diskGB, "disk", 5, "disk in GB")
	return cmd
}
