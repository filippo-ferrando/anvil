package export

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// zstdBin locates the zstd CLI, used to compress/decompress bundles. This is consistent with the
// codebase shelling out to qemu-img/ssh-keygen/setfacl instead of vendoring a compression library.
func zstdBin() (string, error) {
	path, err := exec.LookPath("zstd")
	if err != nil {
		return "", fmt.Errorf("export: zstd not found on PATH (needed to read/write bundles)")
	}
	return path, nil
}

// writeTarZst streams a tar archive built by build into destPath, compressed with zstd.
func writeTarZst(ctx context.Context, destPath string, build func(tw *tar.Writer) error) error {
	zstdPath, err := zstdBin()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("export: creating output dir: %w", err)
	}
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("export: creating %s: %w", destPath, err)
	}
	defer out.Close()

	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, zstdPath, "-q", "-c")
	cmd.Stdin = pr
	cmd.Stdout = out
	cmd.Stderr = os.Stderr

	tarErrCh := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		buildErr := build(tw)
		if closeErr := tw.Close(); buildErr == nil {
			buildErr = closeErr
		}
		if buildErr != nil {
			pw.CloseWithError(buildErr)
		} else {
			pw.Close()
		}
		tarErrCh <- buildErr
	}()

	if err := cmd.Run(); err != nil {
		<-tarErrCh
		return fmt.Errorf("export: compressing bundle: %w", err)
	}
	return <-tarErrCh
}

// extractTarZst decompresses and unpacks bundlePath into destDir.
func extractTarZst(ctx context.Context, bundlePath, destDir string) error {
	zstdPath, err := zstdBin()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return fmt.Errorf("export: creating extract dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, zstdPath, "-d", "-q", "-c", bundlePath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("export: decompressing %s: %w", bundlePath, err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("export: decompressing %s: %w", bundlePath, err)
	}

	extractErr := extractAll(tar.NewReader(stdout), destDir)
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	return waitErr
}

// extractAll writes every entry in tr under destDir, cleaning a leading "/" from entry names so a
// maliciously crafted "../" path can't escape destDir.
func extractAll(tr *tar.Reader, destDir string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("export: reading bundle: %w", err)
		}

		target := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// addBytesToTar writes data as one regular-file entry at archivePath.
func addBytesToTar(tw *tar.Writer, archivePath string, data []byte) error {
	hdr := &tar.Header{Name: archivePath, Mode: 0o640, Size: int64(len(data))}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// addFileToTar copies the file at srcPath into the archive at archivePath.
func addFileToTar(tw *tar.Writer, archivePath, srcPath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = archivePath
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// addDirToTar recursively adds srcDir's contents under archivePrefix.
// Non-regular files (symlinks, sockets, devices) are skipped.
func addDirToTar(tw *tar.Writer, archivePrefix, srcDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := archivePrefix
		if rel != "." {
			name = archivePrefix + "/" + filepath.ToSlash(rel)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name + "/"
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			// Skips symlinks/sockets/devices inside a bind-mounted volume;
			// add real handling if a bundle ever needs one.
			return nil
		}
		return addFileToTar(tw, name, path)
	})
}
