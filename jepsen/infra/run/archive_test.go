package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"testing"
)

// The same sources pack to the same bytes, or every update would build
// again; and what the build needs is there, executables executable.
func TestSourceArchive(t *testing.T) {
	path1, hash1, err := sourceArchive()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path1) })
	_, hash2, err := sourceArchive()
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("the same sources packed twice differ: %s, %s", hash1, hash2)
	}

	f, err := os.Open(path1)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"go.mod": false, "jepsen/node/go.mod": false, "jepsen/project.clj": false, "jepsen/check-env.sh": false}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.ModTime.Unix() != 0 || hdr.Uid != 0 || hdr.Uname != "" {
			t.Errorf("%s: dated or owned: %v, %d %q", hdr.Name, hdr.ModTime, hdr.Uid, hdr.Uname)
		}
		if _, ok := want[hdr.Name]; ok {
			want[hdr.Name] = true
		}
		if hdr.Name == "jepsen/check-env.sh" && hdr.Mode&0o111 == 0 {
			t.Errorf("%s: not executable: %o", hdr.Name, hdr.Mode)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s: not in the archive", name)
		}
	}
}
