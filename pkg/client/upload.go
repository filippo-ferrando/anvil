package client

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// UploadFiles lists what an upload of dir holds, relative to it: in a git work tree what
// git doesn't ignore (`git ls-files`, fromGit), everything otherwise.
func UploadFiles(dir string) (files []string, fromGit bool, err error) {
	if exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run() == nil {
		out, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
		if err != nil {
			return nil, true, fmt.Errorf("listing %s with git: %w", dir, err)
		}
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		return files, true, nil
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, rel)
		return nil
	})
	return files, false, err
}

// UploadChunkSize is the size of the chunks SendUpload hands out, one message each.
const UploadChunkSize = 256 << 10

// SendUpload streams the tar of dir to send in full UploadChunkSize chunks (the last one
// shorter), so a tree of small files isn't one message per tar header and file.
func SendUpload(dir string, send func(chunk []byte) error) error {
	pr, pw := io.Pipe()
	go func() {
		bw := bufio.NewWriterSize(pw, UploadChunkSize)
		err := WriteUploadTar(dir, bw)
		if err == nil {
			err = bw.Flush()
		}
		pw.CloseWithError(err)
	}()
	for {
		buf := make([]byte, UploadChunkSize) // its own buffer: send may keep it
		n, err := io.ReadFull(pr, buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				pr.CloseWithError(serr) // stops the tar writer too
				return serr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("packing %s: %w", dir, err)
		}
	}
}

// WriteUploadTar writes the tar of UploadFiles(dir) that an ephemeral run extracts into /work.
func WriteUploadTar(dir string, w io.Writer) error {
	files, fromGit, err := UploadFiles(dir)
	if err != nil {
		return err
	}
	sort.Strings(files)
	tw := tar.NewWriter(w)
	for _, rel := range files {
		full := filepath.Join(dir, rel)
		info, err := os.Lstat(full)
		if err != nil {
			continue // tracked by git but deleted from the work tree
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(full)
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			continue
		}
		if info.IsDir() && fromGit {
			continue // a submodule: git lists it as one entry, without its files
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err := copyFile(tw, full); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

func copyFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
