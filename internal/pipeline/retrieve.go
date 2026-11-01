package pipeline

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// upload is the tar job j gets in /work: the source, plus the collected files of the
// jobs it retrieves from. The caller removes it when it isn't r.tar.
func (r *run) upload(name string, j *Job, jobs map[string]*Job) (string, error) {
	if len(j.retrieve) == 0 {
		return r.tar, nil
	}
	out := filepath.Join(r.dir, "upload-"+name+".tar")
	err := r.writeUpload(out, j, jobs)
	if err != nil {
		_ = os.Remove(out)
		return "", fmt.Errorf("retrieving files: %w", err)
	}
	return out, nil
}

func (r *run) writeUpload(out string, j *Job, jobs map[string]*Job) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	if err := copyTar(tw, r.tar); err != nil {
		return err
	}
	// Retrieved files come after the source, so they replace source files of the same name.
	for _, spec := range j.retrieve {
		from := jobs[spec.Job]
		for _, d := range from.Distros {
			dir := spec.Dir
			if len(from.Distros) > 1 {
				dir = path.Join(dir, strings.ReplaceAll(d, "/", "_"))
			}
			if err := addTree(tw, filepath.Join(r.dir, "artifacts", spec.Job, d), dir); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Close()
}

// copyTar copies every entry of the tar file src into tw.
func copyTar(tw *tar.Writer, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// addTree adds the files under dir to tw, under prefix. A missing dir adds nothing.
func addTree(tw *tar.Writer, dir, prefix string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = path.Join(prefix, filepath.ToSlash(rel))
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
