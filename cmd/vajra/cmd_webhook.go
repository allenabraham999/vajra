package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// webhookView mirrors models.Webhook. Secret is only populated on the
// create response.
type webhookView struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret,omitempty"`
	Events    []string  `json:"events"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// newWebhookCmd builds the `vajra webhook` subtree.
func newWebhookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Manage outbound notification webhooks",
	}
	cmd.AddCommand(
		newWebhookCreateCmd(),
		newWebhookListCmd(),
		newWebhookDeleteCmd(),
		newWebhookTestCmd(),
	)
	return cmd
}

// newWebhookCreateCmd persists a new webhook target. The HMAC secret is
// returned exactly once.
func newWebhookCreateCmd() *cobra.Command {
	var url, eventList string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a webhook subscription",
		RunE: func(_ *cobra.Command, _ []string) error {
			if url == "" || eventList == "" {
				return fmt.Errorf("--url and --events are required")
			}
			events := strings.Split(eventList, ",")
			for i, e := range events {
				events[i] = strings.TrimSpace(e)
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
			var resp webhookView
			if err := c.do(ctx, "POST", "/v1/webhooks", map[string]any{
				"url": url, "events": events,
			}, &resp); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			out(fmt.Sprintf("created webhook %s", resp.ID))
			out("save this secret — it will not be shown again:")
			out("  " + resp.Secret)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "receiver URL (https:// recommended)")
	cmd.Flags().StringVar(&eventList, "events", "", "comma-separated event names (e.g. sandbox.created,sandbox.stopped)")
	return cmd
}

// newWebhookListCmd shows configured webhooks.
func newWebhookListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List webhooks",
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
			var hooks []webhookView
			if err := c.do(ctx, "GET", "/v1/webhooks", nil, &hooks); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(hooks)
			}
			rows := make([][]string, 0, len(hooks))
			for _, w := range hooks {
				rows = append(rows, []string{
					w.ID, w.URL, strings.Join(w.Events, ","),
					fmt.Sprintf("%t", w.Active),
					w.CreatedAt.Format(time.RFC3339),
				})
			}
			table([]string{"ID", "URL", "EVENTS", "ACTIVE", "CREATED"}, rows)
			return nil
		},
	}
}

// newWebhookDeleteCmd removes a webhook by ID.
func newWebhookDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a webhook by ID",
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
			if err := c.do(ctx, "DELETE", "/v1/webhooks/"+args[0], nil, nil); err != nil {
				return err
			}
			out("deleted " + args[0])
			return nil
		},
	}
}

// newWebhookTestCmd fires a synthetic payload at the configured URL.
func newWebhookTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <id>",
		Short: "Fire a synthetic payload at a webhook",
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
			var resp struct {
				WebhookID string `json:"webhook_id"`
				Delivered bool   `json:"delivered"`
			}
			if err := c.do(ctx, "POST", "/v1/webhooks/"+args[0]+"/test", nil, &resp); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			if resp.Delivered {
				out("delivered ✓")
			} else {
				out("delivery failed (receiver returned non-2xx after retries)")
			}
			return nil
		},
	}
}
