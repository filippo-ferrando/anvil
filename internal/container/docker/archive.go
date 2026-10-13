package docker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ArchiveEntry is one file of a container's filesystem, as Docker's archive API returns it.
type ArchiveEntry struct {
	Name     string // relative to the requested path's parent, e.g. "applications/gimp.desktop"
	Typeflag byte   // tar.TypeReg, tar.TypeDir, tar.TypeSymlink, ...
	Linkname string
	Data     []byte // nil for anything but a regular file of at most maxSize bytes
}

// Archive reads path from container id: one file, one symlink (not followed) or a directory
// tree. ok is false when path doesn't exist.
func (c *Client) Archive(ctx context.Context, id, path string, maxSize int64) ([]ArchiveEntry, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/containers/%s/archive?path=%s", id, url.QueryEscape(path)), nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, statusError(resp)
	}
	var entries []ArchiveEntry
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("docker: reading archive of %s: %w", path, err)
		}
		e := ArchiveEntry{Name: h.Name, Typeflag: h.Typeflag, Linkname: h.Linkname}
		if h.Typeflag == tar.TypeReg && h.Size <= maxSize {
			if e.Data, err = io.ReadAll(tr); err != nil {
				return nil, false, fmt.Errorf("docker: reading %s from archive: %w", h.Name, err)
			}
		}
		entries = append(entries, e)
	}
}
