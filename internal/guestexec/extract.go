package guestexec

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
)

// Extract unpacks the tar stream r into dir. Every entry stays inside dir, symlinks included.
func Extract(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the upload: %w", err)
		}
		name := path.Clean(h.Name)
		if name == "." || name == "/" {
			continue
		}
		mode := os.FileMode(h.Mode).Perm()
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		// An earlier extract's link is replaced, not written through.
		if fi, err := root.Lstat(name); err == nil && (fi.Mode()&os.ModeSymlink != 0 || h.Typeflag == tar.TypeSymlink && !fi.IsDir()) {
			_ = root.Remove(name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, mode|0o700)
		case tar.TypeReg:
			err = extractFile(root, name, mode, tr)
		case tar.TypeSymlink:
			err = root.Symlink(h.Linkname, name)
		default:
			continue // devices, fifos and hard links have no place in an uploaded tree
		}
		if err != nil {
			return fmt.Errorf("extracting %s: %w", h.Name, err)
		}
	}
}

func extractFile(root *os.Root, name string, mode os.FileMode, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Chmod(name, mode) // OpenFile's mode went through the umask
}
