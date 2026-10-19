package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// sourceArchive packs this repository as it is in the working tree: the
// files git tracks, changed or not, and those it does not but does not
// ignore either; not those deleted. The archive is the same for the same
// files, so the same sources build once. Returns its path, in the temporary
// directory, and its SHA-256.
func sourceArchive() (path, hash string, err error) {
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse: %w", err)
	}
	root := strings.TrimSpace(string(top))
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return "", "", fmt.Errorf("git ls-files: %w", err)
	}
	names := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	slices.Sort(names)
	names = slices.Compact(names)

	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		if err := add(tw, root, name); err != nil {
			return "", "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", "", err
	}
	if err := gz.Close(); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	hash = hex.EncodeToString(sum[:])
	path = filepath.Join(os.TempDir(), "fix2-jepsen-src-"+hash[:16]+".tar.gz")
	return path, hash, os.WriteFile(path, buf.Bytes(), 0o644)
}

// add writes one file to the archive, with no owner and no time: a regular
// file, executable or not, or a symbolic link. A file listed but gone,
// deleted in the working tree, is left out.
func add(tw *tar.Writer, root, name string) error {
	p := filepath.Join(root, name)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	hdr := &tar.Header{Name: name, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	switch {
	case fi.Mode().IsRegular():
		hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o644, fi.Size()
		if fi.Mode()&0o111 != 0 {
			hdr.Mode = 0o755
		}
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, target, 0o777
	default:
		// A submodule's directory, say: nothing of it is built.
		return nil
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if int64(len(b)) != hdr.Size {
		return fmt.Errorf("%s changed while archived", name)
	}
	_, err = tw.Write(b)
	return err
}
