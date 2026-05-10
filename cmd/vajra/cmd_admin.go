package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// autoscaleStatus mirrors master.AutoscaleStatus.
type autoscaleStatus struct {
	Enabled      bool `json:"enabled"`
	Scaling      bool `json:"scaling"`
	PendingCount int  `json:"pending_count"`
	NodeCount    int  `json:"node_count"`
	MinNodes     int  `json:"min_nodes"`
	MaxNodes     int  `json:"max_nodes"`
}

// newAdminCmd wires `vajra admin …` — admin-only operations gated on
// the master side by ADMIN_ACCOUNT_ID matching the calling account.
func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administrative operations (admin only)",
	}
	cmd.AddCommand(newAdminAutoscaleCmd())
	return cmd
}

func newAdminAutoscaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autoscale",
		Short: "Inspect or trigger the autoscaler",
	}
	cmd.AddCommand(newAdminAutoscaleStatusCmd(), newAdminAutoscaleTriggerCmd())
	return cmd
}

func newAdminAutoscaleStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show autoscaler status",
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
			var st autoscaleStatus
			if err := c.do(ctx, "GET", "/v1/admin/autoscale", nil, &st); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(st)
			}
			out(fmt.Sprintf("autoscaler  enabled=%v  scaling=%v  pending=%d  nodes=%d  min=%d  max=%d",
				st.Enabled, st.Scaling, st.PendingCount, st.NodeCount, st.MinNodes, st.MaxNodes))
			return nil
		},
	}
}

func newAdminAutoscaleTriggerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trigger",
		Short: "Force a single scale-up event",
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
			var resp map[string]string
			if err := c.do(ctx, "POST", "/v1/admin/autoscale/trigger", nil, &resp); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			out("autoscaler triggered")
			return nil
		},
	}
}
