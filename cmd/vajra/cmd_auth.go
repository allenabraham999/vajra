package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// loginResponse mirrors master's POST /v1/auth/login body.
type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// registerResponse mirrors master's POST /v1/auth/register body.
type registerResponse struct {
	AccountID string `json:"account_id"`
	APIKey    string `json:"api_key"`
}

// newLoginCmd creates the `vajra login` command. Persists JWT + URL into
// ~/.vajra/config.json on success.
func newLoginCmd() *cobra.Command {
	var email, password string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate against vajra-master and store the JWT",
		RunE: func(_ *cobra.Command, _ []string) error {
			if email == "" || password == "" {
				return fmt.Errorf("--email and --password are required")
			}
			c, cfg, err := resolveClient()
			if err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var resp loginResponse
			if err := c.do(ctx, "POST", "/v1/auth/login",
				map[string]string{"email": email, "password": password}, &resp); err != nil {
				return err
			}
			cfg.APIURL = c.baseURL
			cfg.JWT = resp.Token
			cfg.Email = email
			if err := saveConfig(cfg); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			out(fmt.Sprintf("logged in as %s (token expires %s)", email, resp.ExpiresAt.Format(time.RFC3339)))
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "account email")
	cmd.Flags().StringVar(&password, "password", "", "account password")
	return cmd
}

// newRegisterCmd creates the `vajra register` command. The API key is
// surfaced exactly once; we persist it into ~/.vajra/config.json so
// subsequent CLI calls are authed without a separate `login`.
func newRegisterCmd() *cobra.Command {
	var email, password string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Create a new account and store the issued API key",
		RunE: func(_ *cobra.Command, _ []string) error {
			if email == "" || password == "" {
				return fmt.Errorf("--email and --password are required")
			}
			c, cfg, err := resolveClient()
			if err != nil {
				return err
			}
			ctx, cancel := withCtx()
			defer cancel()
			var resp registerResponse
			if err := c.do(ctx, "POST", "/v1/auth/register",
				map[string]string{"email": email, "password": password}, &resp); err != nil {
				return err
			}
			cfg.APIURL = c.baseURL
			cfg.APIKey = resp.APIKey
			cfg.Email = email
			if err := saveConfig(cfg); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp)
			}
			out(fmt.Sprintf("registered account %s", resp.AccountID))
			out(fmt.Sprintf("api key (saved to ~/.vajra/config.json): %s", resp.APIKey))
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "account email")
	cmd.Flags().StringVar(&password, "password", "", "account password (>= 8 chars)")
	return cmd
}
