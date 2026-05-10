package main

import (
	"time"

	"github.com/spf13/cobra"
)

// template mirrors models.Template.
type template struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Hash         string    `json:"hash"`
	RootfsPath   string    `json:"rootfs_path"`
	KernelPath   string    `json:"kernel_path"`
	SnapshotPath string    `json:"snapshot_path"`
	CreatedAt    time.Time `json:"created_at"`
}

// newTemplateCmd builds the `vajra template` subtree.
func newTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "List and manage templates",
	}
	cmd.AddCommand(newTemplateListCmd())
	return cmd
}

func newTemplateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available templates",
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
			var ts []template
			if err := c.do(ctx, "GET", "/v1/templates", nil, &ts); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(ts)
			}
			rows := make([][]string, 0, len(ts))
			for _, t := range ts {
				rows = append(rows, []string{
					t.ID, t.Name, t.Version, t.Hash, t.CreatedAt.Format(time.RFC3339),
				})
			}
			table([]string{"ID", "NAME", "VERSION", "HASH", "CREATED"}, rows)
			return nil
		},
	}
}
