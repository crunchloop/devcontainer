package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/runtime"
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
	if err := tarDirectory(dir, nil, &buf); err != nil {
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
	if err := tarDirectory(a, nil, &bufA); err != nil {
		t.Fatalf("tarDirectory a: %v", err)
	}
	if err := tarDirectory(b, nil, &bufB); err != nil {
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

func TestSyntaxFrontend(t *testing.T) {
	cases := []struct {
		name       string
		dockerfile string
		buildArgs  map[string]string
		want       string
	}{
		{"none", "FROM alpine:3.20\n", nil, ""},
		{"directive", "# syntax=docker/dockerfile:1\nFROM alpine:3.20\n", nil, "docker/dockerfile:1"},
		{"spaced and uppercase key", "#  SYNTAX = docker/dockerfile:1.7-labs  \nFROM alpine\n", nil, "docker/dockerfile:1.7-labs"},
		{"frontend arguments", "# syntax=registry.example.com/fe:1 --opt=x\nFROM alpine\n", nil, "registry.example.com/fe:1"},
		{"after other directives", "# escape=`\n# check=skip=all\n# syntax=docker/dockerfile:1\nFROM alpine\n", nil, "docker/dockerfile:1"},
		{"after shebang", "#!/usr/bin/env buildctl\n# syntax=docker/dockerfile:1\nFROM alpine\n", nil, "docker/dockerfile:1"},
		{"after BOM", "\xef\xbb\xbf# syntax=docker/dockerfile:1\nFROM alpine\n", nil, "docker/dockerfile:1"},
		{"CRLF", "# syntax=docker/dockerfile:1\r\nFROM alpine\r\n", nil, "docker/dockerfile:1"},
		{"c-style comment", "// syntax=docker/dockerfile:1\nFROM alpine\n", nil, "docker/dockerfile:1"},
		{"after a plain comment", "# build the app\n# syntax=docker/dockerfile:1\nFROM alpine\n", nil, ""},
		{"after a blank line", "\n# syntax=docker/dockerfile:1\nFROM alpine\n", nil, ""},
		{"indented", "  # syntax=docker/dockerfile:1\nFROM alpine\n", nil, ""},
		{"after an instruction", "FROM alpine\n# syntax=docker/dockerfile:1\n", nil, ""},
		{"build arg wins", "# syntax=docker/dockerfile:1\nFROM alpine\n", map[string]string{"BUILDKIT_SYNTAX": " docker/dockerfile:1.9 --x"}, "docker/dockerfile:1.9"},
		{"build arg without directive", "FROM alpine\n", map[string]string{"BUILDKIT_SYNTAX": "docker/dockerfile:1"}, "docker/dockerfile:1"},
		{"json", "{\"syntax\": \"registry.example.com/fe:2\", \"stages\": []}\n", nil, "registry.example.com/fe:2"},
		{"json after shebang", "#!/usr/bin/env x\n{\"syntax\": \"registry.example.com/fe:2\"}", nil, "registry.example.com/fe:2"},
		{"json without syntax", "{\"stages\": []}", nil, ""},
		{"json non-string syntax", "{\"syntax\": 1}", nil, ""},
		{"comment directive wins over json", "# syntax=docker/dockerfile:1\n{\"syntax\": \"x\"}", nil, "docker/dockerfile:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := syntaxFrontend([]byte(tc.dockerfile), tc.buildArgs); got != tc.want {
				t.Errorf("syntaxFrontend = %q, want %q", got, tc.want)
			}
		})
	}
}

// dockerfileFixture lays out a workspace for locateDockerfile:
//
//	<root>/ws/.devcontainer/Dockerfile   the config's Dockerfile
//	<root>/ws/app/                       a context that doesn't hold it
//	<root>/outside/secret                a file outside the workspace
type dockerfileFixture struct {
	root, ws, app, dockerfile, secret string
}

const fixtureSecret = "TOP-SECRET-must-not-leak"

func newDockerfileFixture(t *testing.T) dockerfileFixture {
	t.Helper()
	root := t.TempDir()
	f := dockerfileFixture{
		root:       root,
		ws:         filepath.Join(root, "ws"),
		app:        filepath.Join(root, "ws", "app"),
		dockerfile: filepath.Join(root, "ws", ".devcontainer", "Dockerfile"),
		secret:     filepath.Join(root, "outside", "secret"),
	}
	writeFile(t, f.dockerfile, "FROM alpine\n")
	writeFile(t, filepath.Join(f.app, "app.txt"), "app\n")
	writeFile(t, f.secret, fixtureSecret+"\n")
	return f
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocateDockerfile(t *testing.T) {
	f := newDockerfileFixture(t)

	t.Run("in context", func(t *testing.T) {
		cases := []struct {
			name, dockerfile, wantTar, wantBody string
		}{
			// .devcontainer/Dockerfile with "context": ".." (#156).
			{"absolute, context is the workspace", f.dockerfile, ".devcontainer/Dockerfile", "FROM alpine\n"},
			{"relative to context", ".devcontainer/Dockerfile", ".devcontainer/Dockerfile", "FROM alpine\n"},
			// Unreadable on the host: left for the daemon to report.
			{"missing", "sub/Dockerfile", "sub/Dockerfile", ""},
			{"empty defaults to Dockerfile", "", "Dockerfile", ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				name, body, injected, err := locateDockerfile(f.ws, tc.dockerfile, f.ws)
				if err != nil {
					t.Fatal(err)
				}
				if name != tc.wantTar || string(body) != tc.wantBody || injected != nil {
					t.Errorf("got (%q, %q, %v), want (%q, %q, nil)", name, body, injected, tc.wantTar, tc.wantBody)
				}
			})
		}
	})

	t.Run("in context with no workspace root", func(t *testing.T) {
		name, body, injected, err := locateDockerfile(f.ws, f.dockerfile, "")
		if err != nil || name != ".devcontainer/Dockerfile" || string(body) != "FROM alpine\n" || injected != nil {
			t.Fatalf("got (%q, %q, %v, %v)", name, body, injected, err)
		}
	})

	t.Run("outside context, inside workspace is injected", func(t *testing.T) {
		name, body, injected, err := locateDockerfile(f.app, f.dockerfile, f.ws)
		if err != nil {
			t.Fatal(err)
		}
		if injected == nil || injected.Name != name || string(injected.Body) != "FROM alpine\n" || string(body) != "FROM alpine\n" {
			t.Fatalf("injected = %+v, name %q, body %q", injected, name, body)
		}
		if !strings.HasPrefix(name, ".dockerfile.") {
			t.Errorf("name = %q, want .dockerfile.<hash>", name)
		}
		// Deterministic: same content, same name.
		again, _, _, _ := locateDockerfile(f.app, f.dockerfile, f.ws)
		if again != name {
			t.Errorf("name not stable: %q vs %q", name, again)
		}
	})

	t.Run("relative traversal inside workspace is injected", func(t *testing.T) {
		_, _, injected, err := locateDockerfile(f.app, "../.devcontainer/Dockerfile", f.ws)
		if err != nil || injected == nil {
			t.Fatalf("injected = %+v, err = %v", injected, err)
		}
	})

	t.Run("symlink staying inside workspace is injected", func(t *testing.T) {
		link := filepath.Join(f.ws, "Dockerfile.link")
		if err := os.Symlink(f.dockerfile, link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		_, _, injected, err := locateDockerfile(f.app, link, f.ws)
		if err != nil || injected == nil || string(injected.Body) != "FROM alpine\n" {
			t.Fatalf("injected = %+v, err = %v", injected, err)
		}
	})

	t.Run("missing inside workspace is a plain error", func(t *testing.T) {
		_, _, _, err := locateDockerfile(f.app, filepath.Join(f.ws, "nope", "Dockerfile"), f.ws)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want not-exist", err)
		}
	})
}

// Every refused path fails with *runtime.DockerfileRefusedError naming
// the configured path, and never with the file's contents.
func TestLocateDockerfileRefuses(t *testing.T) {
	f := newDockerfileFixture(t)

	escape := filepath.Join(f.ws, "escape")
	if err := os.Symlink(f.secret, escape); err != nil {
		t.Fatal(err)
	}
	inCtxEscape := filepath.Join(f.app, "Dockerfile")
	if err := os.Symlink(f.secret, inCtxEscape); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(f.root, "home")
	writeFile(t, filepath.Join(home, "x"), fixtureSecret+"\n")
	fifo := filepath.Join(f.ws, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(f.ws, "big")
	writeFile(t, big, fixtureSecret+strings.Repeat("#", maxInjectedDockerfileSize))

	cases := []struct {
		name, context, dockerfile, root string
	}{
		{"absolute temp file outside the workspace", f.app, f.secret, f.ws},
		{"absolute traversal out of the workspace", f.app, filepath.Join(f.ws, "..", "outside", "secret"), f.ws},
		{"relative traversal out of the workspace", f.app, "../../outside/secret", f.ws},
		{"symlink in the workspace pointing outside", f.app, escape, f.ws},
		{"in-context symlink pointing outside", f.app, "Dockerfile", f.ws},
		{"HOME outside the workspace", f.app, filepath.Join(home, "x"), f.ws},
		{"missing file outside the workspace", f.app, filepath.Join(f.root, "outside", "nope"), f.ws},
		{"directory", f.app, filepath.Join(f.ws, ".devcontainer"), f.ws},
		{"fifo", f.app, fifo, f.ws},
		{"oversized", f.app, big, f.ws},
		{"outside the context with no workspace root", f.app, f.dockerfile, ""},
	}
	if _, err := os.Stat("/etc/hostname"); err == nil {
		cases = append(cases, struct{ name, context, dockerfile, root string }{"/etc/hostname", f.app, "/etc/hostname", f.ws})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, body, injected, err := locateDockerfile(tc.context, tc.dockerfile, tc.root)
			var refused *runtime.DockerfileRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want *runtime.DockerfileRefusedError", err)
			}
			if name != "" || body != nil || injected != nil {
				t.Errorf("refused path returned (%q, %q, %v)", name, body, injected)
			}
			if refused.Path != tc.dockerfile || !strings.Contains(err.Error(), tc.dockerfile) {
				t.Errorf("error %q does not name %q", err, tc.dockerfile)
			}
			if strings.Contains(err.Error(), fixtureSecret) {
				t.Errorf("error leaks file contents: %q", err)
			}
		})
	}
}

// A directory on the checked path swapped for a symlink to outside the
// workspace after resolveWithin ran must not be followed by the read:
// this is the state the swap leaves, read through the checked result.
func TestReadDockerfileRefusesAncestorSwap(t *testing.T) {
	f := newDockerfileFixture(t)
	writeFile(t, filepath.Join(f.ws, "sub", "secret"), "FROM alpine\n")
	at, err := resolveWithin("sub/secret", filepath.Join(f.ws, "sub", "secret"), f.ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.ws, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(f.secret), filepath.Join(f.ws, "sub")); err != nil {
		t.Fatal(err)
	}

	body, err := readDockerfile("sub/secret", at)
	var refused *runtime.DockerfileRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, body %q; want *runtime.DockerfileRefusedError", err, body)
	}
	if body != nil || strings.Contains(err.Error(), fixtureSecret) {
		t.Errorf("read through the swapped directory: body %q, err %q", body, err)
	}
}

// A context inside the workspace is a folder the repository controls,
// so an in-context Dockerfile is confined to the workspace root, not
// to the context: swapping the context for a symlink to outside after
// the check must not carry the read with it.
func TestReadDockerfileRefusesContextSwap(t *testing.T) {
	f := newDockerfileFixture(t)
	writeFile(t, filepath.Join(f.app, "Dockerfile"), "FROM alpine\n")
	writeFile(t, filepath.Join(filepath.Dir(f.secret), "Dockerfile"), fixtureSecret+"\n")
	at, err := resolveWithin("Dockerfile", filepath.Join(f.app, "Dockerfile"), f.ws, f.app)
	if err != nil {
		t.Fatal(err)
	}
	if realWS, _ := filepath.EvalSymlinks(f.ws); at.Root != realWS {
		t.Fatalf("Root = %q, want the workspace %q", at.Root, realWS)
	}
	if err := os.RemoveAll(f.app); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(f.secret), f.app); err != nil {
		t.Fatal(err)
	}

	body, err := readDockerfile("Dockerfile", at)
	var refused *runtime.DockerfileRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, body %q; want *runtime.DockerfileRefusedError", err, body)
	}
	if body != nil || strings.Contains(err.Error(), fixtureSecret) {
		t.Errorf("read through the swapped context: body %q, err %q", body, err)
	}
}

// A refused Dockerfile fails BuildImage before any daemon call: the
// Runtime here has no client, so reaching the daemon would panic.
func TestBuildImageRefusesBeforeDaemon(t *testing.T) {
	f := newDockerfileFixture(t)
	events := make(chan runtime.BuildEvent, 16)
	_, err := (&Runtime{}).BuildImage(context.Background(), runtime.BuildSpec{
		ContextPath:   f.app,
		Dockerfile:    f.secret,
		WorkspaceRoot: f.ws,
		Tag:           "refused:test",
	}, events)
	var refused *runtime.DockerfileRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *runtime.DockerfileRefusedError", err)
	}
	if strings.Contains(err.Error(), fixtureSecret) {
		t.Errorf("error leaks file contents: %q", err)
	}
	close(events)
	for ev := range events {
		t.Errorf("unexpected event %+v", ev)
	}
}

func TestTarDirectoryAppendsExtra(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tarDirectory(dir, &tarFile{Name: ".dockerfile.abc", Body: []byte("FROM alpine\n")}, &buf); err != nil {
		t.Fatalf("tarDirectory: %v", err)
	}
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		body, _ := io.ReadAll(tr)
		got[hdr.Name] = string(body)
		if !hdr.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("%s: ModTime not epoch", hdr.Name)
		}
	}
	want := map[string]string{"file.txt": "hello", ".dockerfile.abc": "FROM alpine\n"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}
