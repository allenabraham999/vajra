package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// node mirrors models.Node.
type node struct {
	ID            string    `json:"id"`
	ClusterID     string    `json:"cluster_id"`
	Hostname      string    `json:"hostname"`
	IP            string    `json:"ip"`
	State         string    `json:"state"`
	Capacity      nodeCap   `json:"capacity"`
	UsedResources nodeUse   `json:"used_resources"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

type nodeCap struct {
	TotalCPU      int `json:"total_cpu"`
	TotalMemoryMB int `json:"total_memory_mb"`
	TotalDiskGB   int `json:"total_disk_gb"`
}

type nodeUse struct {
	UsedCPU      int `json:"used_cpu"`
	UsedMemoryMB int `json:"used_memory_mb"`
	UsedDiskGB   int `json:"used_disk_gb"`
}

// newNodeCmd builds the `vajra node` subtree (admin gated server-side).
func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Inspect and manage nodes (admin)",
	}
	cmd.AddCommand(newNodeListCmd(), newNodeDrainCmd())
	return cmd
}

func newNodeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List nodes",
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
			var nodes []node
			if err := c.do(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(nodes)
			}
			rows := make([][]string, 0, len(nodes))
			for _, n := range nodes {
				rows = append(rows, []string{
					n.ID, n.Hostname, stateColor(n.State),
					fmt.Sprintf("%d/%d", n.UsedResources.UsedCPU, n.Capacity.TotalCPU),
					fmt.Sprintf("%d/%d", n.UsedResources.UsedMemoryMB, n.Capacity.TotalMemoryMB),
				})
			}
			table([]string{"ID", "HOSTNAME", "STATE", "CPU", "MEM (MB)"}, rows)
			return nil
		},
	}
}

func newNodeDrainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drain <id>",
		Short: "Mark a node as DRAINING",
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
			var resp map[string]string
			if err := c.do(ctx, "POST", "/v1/nodes/"+args[0]+"/drain", nil, &resp); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			out("node " + args[0] + " — " + stateColor("DRAINING"))
			return nil
		},
	}
}
