package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// apiKeyView mirrors master's listAPIKeys row shape. Only id+name+
// created_at are returned; the master never echoes the raw secret.
type apiKeyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// createAPIKeyResponse mirrors master's createAPIKey body — the raw key
// is appended.
type createAPIKeyResponse struct {
	apiKeyView
	Key string `json:"key"`
}

// newAPIKeyCmd builds the `vajra api-key` subtree (create/list/revoke).
func newAPIKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api-key",
		Short: "Manage API keys",
	}
	cmd.AddCommand(
		newAPIKeyCreateCmd(),
		newAPIKeyListCmd(),
		newAPIKeyRevokeCmd(),
	)
	return cmd
}

// newAPIKeyCreateCmd issues a new API key. The raw secret is shown only
// once — the master only persists its hash.
func newAPIKeyCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new API key",
		RunE: func(_ *cobra.Command, _ []string) error {
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
			var resp createAPIKeyResponse
			if err := c.do(ctx, "POST", "/v1/api-keys",
				map[string]string{"name": name}, &resp); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			out(fmt.Sprintf("created api key %s (%s)", resp.ID, resp.Name))
			out("save this — it will not be shown again:")
			out("  " + resp.Key)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human-readable name for the key")
	return cmd
}

// newAPIKeyListCmd lists keys for the calling account.
func newAPIKeyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API keys",
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
			var keys []apiKeyView
			if err := c.do(ctx, "GET", "/v1/api-keys", nil, &keys); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(keys)
			}
			rows := make([][]string, 0, len(keys))
			for _, k := range keys {
				rows = append(rows, []string{k.ID, k.Name, k.CreatedAt.Format(time.RFC3339)})
			}
			table([]string{"ID", "NAME", "CREATED"}, rows)
			return nil
		},
	}
}

// newAPIKeyRevokeCmd deletes a key by ID.
func newAPIKeyRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API key by ID",
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
			if err := c.do(ctx, "DELETE", "/v1/api-keys/"+args[0], nil, nil); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(map[string]string{"id": args[0], "status": "revoked"})
			}
			out("revoked " + args[0])
			return nil
		},
	}
}
