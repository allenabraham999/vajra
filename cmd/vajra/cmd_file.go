package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// fileTransferTimeout caps a single upload/download. Files can be big;
// 30 minutes is well above what we expect a 1 GiB transfer to take.
const fileTransferTimeout = 30 * time.Minute

// fileEntry mirrors the agent's file list rows.
type fileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// listFilesResponse is the master's listFiles wrapper.
type listFilesResponse struct {
	Entries []fileEntry `json:"entries"`
}

// newFileCmd builds the `vajra file` subtree.
func newFileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "file",
		Short: "Upload, download, and list files in a sandbox",
	}
	cmd.AddCommand(
		newFileUploadCmd(),
		newFileDownloadCmd(),
		newFileListCmd(),
	)
	return cmd
}

func newFileUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <sandbox-id> <local-path> <remote-path>",
		Short: "Upload a local file into a sandbox",
		Args:  cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			id, local, remote := args[0], args[1], args[2]
			f, err := os.Open(local)
			if err != nil {
				return fmt.Errorf("open local: %w", err)
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				return fmt.Errorf("stat local: %w", err)
			}
			if st.IsDir() {
				return errors.New("uploading directories is not supported")
			}
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), fileTransferTimeout)
			defer cancel()
			headers := map[string]string{
				"X-Vajra-Path": remote,
				"X-Vajra-Mode": strconv.FormatUint(uint64(st.Mode().Perm()), 10),
			}
			if err := c.streamPut(ctx, "POST", "/v1/sandboxes/"+id+"/files/upload",
				headers, st.Size(), f); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(map[string]any{"ok": true, "size": st.Size()})
			}
			out(fmt.Sprintf("uploaded %d bytes → %s", st.Size(), remote))
			return nil
		},
	}
}

func newFileDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <sandbox-id> <remote-path> <local-path>",
		Short: "Download a file from a sandbox",
		Args:  cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			id, remote, local := args[0], args[1], args[2]
			c, _, err := resolveClient()
			if err != nil {
				return err
			}
			if err := requireAuth(c); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), fileTransferTimeout)
			defer cancel()
			path := "/v1/sandboxes/" + id + "/files/download?path=" + url.QueryEscape(remote)
			resp, err := c.streamGet(ctx, path)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			out, err := os.Create(local)
			if err != nil {
				return fmt.Errorf("create local: %w", err)
			}
			defer out.Close()
			n, err := io.Copy(out, resp.Body)
			if err != nil {
				return fmt.Errorf("write local: %w", err)
			}
			if gFlags.asJSON {
				return printJSON(map[string]any{"ok": true, "size": n, "path": local})
			}
			fmt.Fprintf(os.Stdout, "downloaded %d bytes → %s\n", n, local)
			return nil
		},
	}
}

func newFileListCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "list <sandbox-id>",
		Short: "List files in a sandbox directory",
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
			path := "/v1/sandboxes/" + args[0] + "/files/list"
			if dir != "" {
				path += "?dir=" + url.QueryEscape(dir)
			}
			var resp listFilesResponse
			if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
				return err
			}
			if gFlags.asJSON {
				return printJSON(resp.Entries)
			}
			rows := make([][]string, 0, len(resp.Entries))
			for _, e := range resp.Entries {
				kind := "file"
				if e.IsDir {
					kind = "dir"
				}
				rows = append(rows, []string{
					kind, e.Name,
					fmt.Sprintf("%d", e.Size),
					fmt.Sprintf("%o", e.Mode),
					e.ModTime.Format(time.RFC3339),
				})
			}
			table([]string{"TYPE", "NAME", "SIZE", "MODE", "MTIME"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "/", "directory to list")
	return cmd
}
