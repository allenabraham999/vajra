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

// templatePoolStats mirrors agent.TemplatePoolStats — one template's
// warm pool. nodePoolStats mirrors agent.NodePoolStats. Kept as their own
// types so the CLI doesn't pull internal/agent into its build graph.
type templatePoolStats struct {
	TemplateHash string `json:"template_hash"`
	TemplateID   string `json:"template_id,omitempty"`
	Available    int    `json:"available"`
	Warming      int    `json:"warming"`
	TargetSize   int    `json:"target_size"`
	InUse        int    `json:"in_use"`
	HitsLastHour int    `json:"hits_last_hour"`
	TotalHits    int64  `json:"total_hits"`
}

type nodePoolStats struct {
	Capacity    int                 `json:"capacity"`
	TotalWarm   int                 `json:"total_warm"`
	TotalHits   int64               `json:"total_hits"`
	TotalMisses int64               `json:"total_misses"`
	Templates   []templatePoolStats `json:"templates"`
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
			var st nodePoolStats
			if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			if gFlags.asJSON {
				return printJSON(st)
			}
			rate := 0.0
			if total := st.TotalHits + st.TotalMisses; total > 0 {
				rate = 100.0 * float64(st.TotalHits) / float64(total)
			}
			out(fmt.Sprintf("pool  templates=%d  warm=%d/%d  hits=%d  misses=%d  hit_rate=%.1f%%",
				len(st.Templates), st.TotalWarm, st.Capacity,
				st.TotalHits, st.TotalMisses, rate))
			if len(st.Templates) == 0 {
				out("  (no template pools — pool disabled or not yet warmed)")
			}
			for _, tp := range st.Templates {
				id := tp.TemplateID
				if id == "" {
					id = tp.TemplateHash
				}
				out(fmt.Sprintf("  %-40s available=%d warming=%d target=%d in_use=%d hits/hr=%d",
					id, tp.Available, tp.Warming, tp.TargetSize, tp.InUse, tp.HitsLastHour))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentURL, "agent-url", "http://localhost:9000", "vajra-agent base URL")
	return cmd
}
