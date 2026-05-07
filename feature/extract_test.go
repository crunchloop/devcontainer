package feature

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type tarEntry struct {
	name     string
	mode     int64
	body     string
	typeflag byte
	linkname string
}

func makeTarball(t *testing.T, entries []tarEntry, gzipped bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var tw *tar.Writer
	var gz *gzip.Writer
	if gzipped {
		gz = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gz)
	} else {
		tw = tar.NewWriter(&buf)
	}
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: flag,
			Linkname: e.linkname,
		}
		if flag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg && e.body != "" {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestExtractTarball_Gzipped(t *testing.T) {
	dst := t.TempDir()
	body := makeTarball(t, []tarEntry{
		{name: "devcontainer-feature.json", mode: 0o644, body: `{"id":"x"}`},
		{name: "install.sh", mode: 0o755, body: "#!/bin/sh\necho hi\n"},
		{name: "lib/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "lib/util.sh", mode: 0o644, body: "echo util"},
	}, true)

	if err := extractTarball(bytes.NewReader(body), dst); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Files exist with right contents.
	for _, f := range []struct{ path, want string }{
		{"devcontainer-feature.json", `{"id":"x"}`},
		{"install.sh", "#!/bin/sh\necho hi\n"},
		{"lib/util.sh", "echo util"},
	} {
		got, err := os.ReadFile(filepath.Join(dst, f.path))
		if err != nil {
			t.Errorf("read %s: %v", f.path, err)
			continue
		}
		if string(got) != f.want {
			t.Errorf("%s = %q, want %q", f.path, got, f.want)
		}
	}

	// install.sh should be executable.
	info, err := os.Stat(filepath.Join(dst, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("install.sh not executable: mode=%v", info.Mode())
	}
}

func TestExtractTarball_PlainTarRejected(t *testing.T) {
	dst := t.TempDir()
	body := makeTarball(t, []tarEntry{
		{name: "f.txt", mode: 0o644, body: "hello"},
	}, false)
	if err := extractTarball(bytes.NewReader(body), dst); err == nil {
		t.Fatal("expected gzip error for plain tar input")
	}
}

func TestExtractTarball_RejectsPathTraversal(t *testing.T) {
	dst := t.TempDir()
	body := makeTarball(t, []tarEntry{
		{name: "../escape.txt", mode: 0o644, body: "naughty"},
	}, true)
	if err := extractTarball(bytes.NewReader(body), dst); err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestExtractTarball_RejectsAbsolutePath(t *testing.T) {
	dst := t.TempDir()
	body := makeTarball(t, []tarEntry{
		{name: "/etc/passwd", mode: 0o644, body: "evil"},
	}, true)
	if err := extractTarball(bytes.NewReader(body), dst); err == nil {
		t.Fatal("expected error for absolute path")
	}
}

func TestExtractTarball_RejectsEscapingSymlink(t *testing.T) {
	dst := t.TempDir()
	body := makeTarball(t, []tarEntry{
		{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	}, true)
	if err := extractTarball(bytes.NewReader(body), dst); err == nil {
		t.Fatal("expected error for symlink escaping destination")
	}
}
