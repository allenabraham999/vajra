// Package master — handlers_template_download.go serves a built
// template's image files to node agents so they can populate their
// local cache on demand.
//
// When a sandbox is scheduled onto a node that has never cached the
// requested template, the agent fetches it from here. The response is a
// single zstd-compressed tar carrying the rootfs, the guest kernel, and
// the Cloud Hypervisor snapshot directory — everything the agent's VMM
// needs to restore a sandbox. The endpoint is mounted under /internal
// and gated by the agent shared secret, so it is not customer-facing.
package master

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/allenabraham999/vajra/internal/store"
	"github.com/klauspost/compress/zstd"
)

// DefaultTemplatesDir is where vajra-master stores built template image
// files, one content-hash subdirectory per template. The builder stages
// freshly built images here and the download endpoint streams them.
const DefaultTemplatesDir = "/var/lib/vajra/templates"

// templateSnapshotFiles are the non-rootfs files every distributable
// template directory must contain. The rootfs is resolved separately
// because either the raw or the converted form is acceptable.
var templateSnapshotFiles = []string{
	"vmlinux",
	"snapshot/config.json",
	"snapshot/memory-ranges",
	"snapshot/state.json",
}

// templatesRoot returns the configured template directory, or the
// compiled-in default when unset.
func (h *Handlers) templatesRoot() string {
	if h.TemplatesDir != "" {
		return h.TemplatesDir
	}
	return DefaultTemplatesDir
}

// templateSearchDirs returns the parent directories downloadTemplate
// searches, in priority order, for a template's <hash>/ bundle: the
// primary templates directory first (where master's builder stages
// images), then any TemplateSourceDirs fallbacks. A co-located
// deployment wires the node agent's image cache in as a fallback so
// bootstrap/default templates the builder never produced (e.g. the
// stock ubuntu-noble) can still be distributed on demand.
func (h *Handlers) templateSearchDirs() []string {
	dirs := make([]string, 0, 1+len(h.TemplateSourceDirs))
	dirs = append(dirs, h.templatesRoot())
	return append(dirs, h.TemplateSourceDirs...)
}

// resolveTemplateBundleForHash locates a complete template bundle for
// hash by checking each search directory in order, returning the bundle
// from the first directory that holds every required file. When none
// do, it returns the error from the primary (templates) directory — the
// most actionable reason for the agent to surface.
func (h *Handlers) resolveTemplateBundleForHash(hash string) ([]bundleFile, error) {
	var primaryErr error
	for i, root := range h.templateSearchDirs() {
		files, err := resolveTemplateBundle(filepath.Join(root, hash))
		if err == nil {
			return files, nil
		}
		if i == 0 {
			primaryErr = err
		}
	}
	return nil, primaryErr
}

// downloadTemplate streams a built template's image files to a node
// agent as a zstd-compressed tar (application/x-tar-zst). The {id} is a
// template registry ID; it is resolved to a content hash, which names
// the on-disk directory under templatesRoot().
//
// A 404 here is the agent's signal that the template was never staged
// on this master — the agent surfaces the body verbatim as the reason a
// sandbox failed to come up, instead of an opaque "not in cache".
func (h *Handlers) downloadTemplate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing template id")
		return
	}
	tmpl, err := h.Store.Templates().GetByIDUnscoped(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "template not found")
			return
		}
		h.log().Error("downloadTemplate: lookup", "err", err, "template_id", id)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	files, err := h.resolveTemplateBundleForHash(tmpl.Hash)
	if err != nil {
		h.log().Warn("downloadTemplate: bundle incomplete",
			"template", tmpl.Name, "hash", tmpl.Hash, "err", err)
		writeErr(w, http.StatusNotFound,
			fmt.Sprintf("template %q not staged on master: %v", tmpl.Name, err))
		return
	}
	w.Header().Set("Content-Type", "application/x-tar-zst")
	if err := streamTemplateBundle(w, files); err != nil {
		// The response body is already (partly) on the wire, so the
		// status can no longer change — log and let the agent's tar
		// reader fail on the truncated stream.
		h.log().Error("downloadTemplate: stream", "hash", tmpl.Hash, "err", err)
	}
}

// bundleFile pairs an on-disk path with the name it takes inside the tar.
type bundleFile struct {
	diskPath string
	tarName  string
}

// resolveTemplateBundle checks that dir holds every file an agent needs
// to restore a sandbox and returns the (diskPath, tarName) pairs to tar
// up. rootfs.raw is preferred; rootfs.qcow2 is accepted as a fallback
// for templates whose raw form was already converted away on the master.
func resolveTemplateBundle(dir string) ([]bundleFile, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("template directory missing")
	}
	var files []bundleFile
	rawPath := filepath.Join(dir, "rootfs.raw")
	qcowPath := filepath.Join(dir, "rootfs.qcow2")
	switch {
	case fileExists(rawPath):
		files = append(files, bundleFile{rawPath, "rootfs.raw"})
	case fileExists(qcowPath):
		files = append(files, bundleFile{qcowPath, "rootfs.qcow2"})
	default:
		return nil, errors.New("rootfs missing")
	}
	for _, name := range templateSnapshotFiles {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if !fileExists(p) {
			return nil, fmt.Errorf("%s missing", name)
		}
		files = append(files, bundleFile{p, name})
	}
	return files, nil
}

// fileExists reports whether p is an existing regular file.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// streamTemplateBundle writes files to w as a single zstd-compressed tar.
func streamTemplateBundle(w io.Writer, files []bundleFile) error {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)
	for _, f := range files {
		if err := writeTarFile(tw, f); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return fmt.Errorf("close tar: %w", err)
	}
	return zw.Close()
}

// writeTarFile copies one file into the tar with a relative name so the
// agent extracts it straight into <cache>/<hash>/.
func writeTarFile(tw *tar.Writer, f bundleFile) error {
	in, err := os.Open(f.diskPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", f.tarName, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.tarName, err)
	}
	hdr := &tar.Header{
		Name:     f.tarName,
		Mode:     0o644,
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", f.tarName, err)
	}
	if _, err := io.Copy(tw, in); err != nil {
		return fmt.Errorf("tar body %s: %w", f.tarName, err)
	}
	return nil
}
