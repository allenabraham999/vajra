package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// poolStats mirrors agent.PoolStats. Kept as its own type so the CLI
// doesn't pull internal/agent into its build graph.
type poolStats struct {
	MinSize      int     `json:"min_size"`
	MaxSize      int     `json:"max_size"`
	TargetSize   int     `json:"target_size"`
	Available    int     `json:"available"`
	Warming      int     `json:"warming"`
	TotalHits    int64   `json:"total_hits"`
	TotalMisses  int64   `json:"total_misses"`
	TotalCreated int64   `json:"total_created"`
	HitRatePct   float64 `json:"hit_rate_pct"`
	Template     string  `json:"template"`
}

// newPoolCmd wires `vajra pool …`. Unlike most subcommands the pool API
// lives on each node's agent, not the master, so the user supplies the
// agent URL with --agent-url (default http://localhost:9000 matches the
// agent's bind address).
func newPoolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Inspect the local agent's pre-warm pool",
	}
	cmd.AddCommand(newPoolStatsCmd())
	return cmd
}

func newPoolStatsCmd() *cobra.Command {
	var agentURL string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show pre-warm pool stats from a vajra-agent",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			base := strings.TrimRight(agentURL, "/")
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/pool/stats", nil)
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("http: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
			}
			var st poolStats
			if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			if gFlags.asJSON {
				return printJSON(st)
			}
			template := st.Template
			if template == "" {
				template = "(pool disabled)"
			}
			out(fmt.Sprintf("pool  template=%s  available=%d  warming=%d  target=%d (min=%d max=%d)",
				template, st.Available, st.Warming, st.TargetSize, st.MinSize, st.MaxSize))
			out(fmt.Sprintf("hits=%d  misses=%d  created=%d  hit_rate=%.1f%%",
				st.TotalHits, st.TotalMisses, st.TotalCreated, st.HitRatePct))
			return nil
		},
	}
	cmd.Flags().StringVar(&agentURL, "agent-url", "http://localhost:9000", "vajra-agent base URL")
	return cmd
}
