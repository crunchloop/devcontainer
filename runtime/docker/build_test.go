package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestExtractBaseImages(t *testing.T) {
	cases := []struct {
		name       string
		dockerfile string
		buildArgs  map[string]string
		want       []string
	}{
		{
			name: "literal FROM",
			dockerfile: `FROM alpine:3.20
RUN echo hi`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "ARG default substituted into FROM",
			dockerfile: `ARG BASE=alpine:3.20
FROM $BASE
RUN echo hi`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "buildArgs override ARG default",
			dockerfile: `ARG BASE=alpine:3.20
FROM ${BASE}`,
			buildArgs: map[string]string{"BASE": "debian:bookworm-slim"},
			want:      []string{"debian:bookworm-slim"},
		},
		{
			name: "FROM AS stage; subsequent FROM refers to stage",
			dockerfile: `FROM alpine:3.20 AS builder
RUN echo a > /a
FROM builder
RUN cat /a`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "multi-stage with distinct base images",
			dockerfile: `FROM golang:1.25 AS build
FROM alpine:3.20`,
			want: []string{"golang:1.25", "alpine:3.20"},
		},
		{
			name:       "FROM with --platform flag",
			dockerfile: `FROM --platform=linux/amd64 alpine:3.20`,
			want:       []string{"alpine:3.20"},
		},
		{
			name: "unresolved ARG dropped",
			dockerfile: `FROM $UNDEFINED_BASE
FROM alpine:3.20`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "comments and blank lines ignored",
			dockerfile: `# comment
   # indented comment

FROM alpine:3.20`,
			want: []string{"alpine:3.20"},
		},
		{
			name:       "lowercase from is recognized",
			dockerfile: `from alpine:3.20`,
			want:       []string{"alpine:3.20"},
		},
		{
			name:       "empty input",
			dockerfile: "",
			want:       nil,
		},
		{
			name: "dedup repeated FROMs",
			dockerfile: `FROM alpine:3.20 AS a
FROM alpine:3.20 AS b`,
			want: []string{"alpine:3.20"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBaseImages(tc.dockerfile, tc.buildArgs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractBaseImages = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTarDirectoryNormalizesMetadata guards against the BuildKit
// COPY-cache regression caused by wall-clock mtimes in synthesized
// build contexts (uid-reconcile, etc.) leaking into the tar stream and
// perturbing the vertex digest of byte-identical content.
func TestTarDirectoryNormalizesMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	var buf bytes.Buffer
	if err := tarDirectory(dir, &buf); err != nil {
		t.Fatalf("tarDirectory: %v", err)
	}

	tr := tar.NewReader(&buf)
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		entries++
		if !hdr.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("%s: ModTime not epoch: %v", hdr.Name, hdr.ModTime)
		}
		if !hdr.AccessTime.IsZero() {
			t.Errorf("%s: AccessTime not zero: %v", hdr.Name, hdr.AccessTime)
		}
		if !hdr.ChangeTime.IsZero() {
			t.Errorf("%s: ChangeTime not zero: %v", hdr.Name, hdr.ChangeTime)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Errorf("%s: uid/gid not zero: uid=%d gid=%d", hdr.Name, hdr.Uid, hdr.Gid)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: uname/gname not empty: uname=%q gname=%q", hdr.Name, hdr.Uname, hdr.Gname)
		}
	}
	if entries == 0 {
		t.Fatal("no tar entries read")
	}
}

// TestTarDirectoryDeterministic asserts that taring the same content
// twice with diverging wall-clock mtimes produces byte-identical
// streams — the property BuildKit's COPY cache relies on.
func TestTarDirectoryDeterministic(t *testing.T) {
	mkContext := func(t *testing.T, mtime time.Time) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "uid-fix.sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		return dir
	}

	a := mkContext(t, time.Unix(1_700_000_000, 0))
	b := mkContext(t, time.Unix(1_800_000_000, 0))

	var bufA, bufB bytes.Buffer
	if err := tarDirectory(a, &bufA); err != nil {
		t.Fatalf("tarDirectory a: %v", err)
	}
	if err := tarDirectory(b, &bufB); err != nil {
		t.Fatalf("tarDirectory b: %v", err)
	}
	if !bytes.Equal(bufA.Bytes(), bufB.Bytes()) {
		t.Fatalf("tar streams differ despite identical content (mtime leaked)")
	}
}

func TestSubstituteArgs(t *testing.T) {
	args := map[string]string{"X": "alpine", "Y": "3.20"}
	cases := []struct {
		in, want string
	}{
		{"$X:$Y", "alpine:3.20"},
		{"${X}:${Y}", "alpine:3.20"},
		{"prefix-$X-suffix", "prefix-alpine-suffix"},
		{"$UNKNOWN", "$UNKNOWN"},
		{"${UNKNOWN}", "${UNKNOWN}"},
		{"$", "$"},
		{"no-vars-here", "no-vars-here"},
	}
	for _, tc := range cases {
		got := substituteArgs(tc.in, args)
		if got != tc.want {
			t.Errorf("substituteArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
