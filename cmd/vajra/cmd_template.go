package main

import (
	"fmt"
	"os"
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

// build is the JSON shape returned by GET /v1/templates/builds/{id}.
type build struct {
	ID              string     `json:"id"`
	AccountID       string     `json:"account_id"`
	TemplateName    string     `json:"template_name"`
	TemplateVersion string     `json:"template_version"`
	Status          string     `json:"status"`
	TemplateID      *string    `json:"template_id,omitempty"`
	Error           *string    `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// newTemplateCmd builds the `vajra template` subtree.
func newTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "List and manage templates",
	}
	cmd.AddCommand(newTemplateListCmd())
	cmd.AddCommand(newTemplateBuildCmd())
	cmd.AddCommand(newTemplateBuildStatusCmd())
	return cmd
}

// newTemplateBuildCmd kicks off an async "Dockerfile → Template" build.
// By default it polls until the build finishes; --no-wait returns the
// build ID immediately so the user can script status checks.
func newTemplateBuildCmd() *cobra.Command {
	var dockerfilePath, name, version string
	var wait bool
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a custom template from a Dockerfile",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			if dockerfilePath == "" || name == "" || version == "" {
				return fmt.Errorf("--dockerfile, --name, and --version are required")
			}
			content, err := os.ReadFile(dockerfilePath)
			if err != nil {
				return fmt.Errorf("read dockerfile: %w", err)
			}
			ctx, cancel := withCtx()
			defer cancel()
			var accepted struct {
				BuildID string `json:"build_id"`
				Status  string `json:"status"`
			}
			if err := c.do(ctx, "POST", "/v1/templates/build", map[string]any{
				"name":       name,
				"version":    version,
				"dockerfile": string(content),
			}, &accepted); err != nil {
				return err
			}
			out(fmt.Sprintf("build %s queued (status=%s)", accepted.BuildID, accepted.Status))
			if !wait {
				return nil
			}
			return pollBuild(c, accepted.BuildID)
		},
	}
	cmd.Flags().StringVar(&dockerfilePath, "dockerfile", "", "path to Dockerfile (required)")
	cmd.Flags().StringVar(&name, "name", "", "template name (required)")
	cmd.Flags().StringVar(&version, "version", "", "template version (required)")
	cmd.Flags().BoolVar(&wait, "wait", true, "poll until the build finishes")
	return cmd
}

// newTemplateBuildStatusCmd lets the user check a build ID directly.
func newTemplateBuildStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "build-status [build-id]",
		Short: "Show status of a template build",
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
			var b build
			if err := c.do(ctx, "GET", "/v1/templates/builds/"+args[0], nil, &b); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(b)
			}
			out(fmt.Sprintf("%s — %s (template_name=%s version=%s)", b.ID, b.Status, b.TemplateName, b.TemplateVersion))
			if b.TemplateID != nil {
				out("template_id: " + *b.TemplateID)
			}
			if b.Error != nil {
				out("error: " + *b.Error)
			}
			return nil
		},
	}
}

// pollBuild polls every 2s until the build reaches a terminal state or
// the CLI timeout expires.
func pollBuild(c *Client, buildID string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		ctx, cancel := withCtx()
		var b build
		err := c.do(ctx, "GET", "/v1/templates/builds/"+buildID, nil, &b)
		cancel()
		if err != nil {
			return err
		}
		switch b.Status {
		case "COMPLETED":
			out("✓ build COMPLETED")
			if b.TemplateID != nil {
				out("template_id: " + *b.TemplateID)
			}
			return nil
		case "FAILED":
			if b.Error != nil {
				return fmt.Errorf("build failed: %s", *b.Error)
			}
			return fmt.Errorf("build failed")
		}
		out("…" + b.Status)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out polling build %s", buildID)
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
