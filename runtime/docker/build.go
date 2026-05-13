package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

// BuildImage builds an image from a build context directory and tags
// it. Implements runtime.Runtime.BuildImage.
//
// Streaming progress messages are mapped onto the events channel as
// runtime.BuildEvents (drop-on-full). Build failures surface as a
// non-nil error including any structured error returned by the daemon.
//
// BuildKit is required. The classic builder synthesizes one
// intermediate container per Dockerfile step and routes every
// container API through the daemon's authorization pipeline, which
// — behind an authz plugin — turns a sub-second build into a
// multi-minute one (~140× slowdown observed in production). BuildKit
// uses a single streaming session and is unaffected. Docker Engine
// has shipped with BuildKit enabled by default since 23.0 (Feb 2023);
// requiring it here is in line with the lib's modern-spec stance.
func (r *Runtime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	if spec.ContextPath == "" {
		return runtime.ImageRef{}, fmt.Errorf("BuildImage: spec.ContextPath required")
	}
	if spec.Tag == "" {
		return runtime.ImageRef{}, fmt.Errorf("BuildImage: spec.Tag required")
	}

	// Stream the context directory as a tar over an io.Pipe so we
	// don't buffer it all in memory.
	pr, pw := io.Pipe()
	go func() {
		err := tarDirectory(spec.ContextPath, pw)
		_ = pw.CloseWithError(err)
	}()

	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	args := make(map[string]*string, len(spec.Args))
	for k, v := range spec.Args {
		v := v
		args[k] = &v
	}

	// Pre-pull base images so BuildKit can resolve them from the local
	// image store. BuildKit refuses remote metadata resolution without
	// an active session ("no active sessions" — the session is the
	// callback channel for registry credentials, mandatory even for
	// anonymous public pulls). Pulling via the classic ImagePull API
	// (which is unaffected by the session requirement) seeds the local
	// store; BuildKit then picks up the cached manifest instead of
	// reaching for the registry.
	prePullBaseImages(ctx, r, spec.ContextPath, dockerfile, spec.Args, events)

	res, err := r.api.ImageBuild(ctx, pr, client.ImageBuildOptions{
		Dockerfile: dockerfile,
		Tags:       []string{spec.Tag},
		BuildArgs:  args,
		Target:     spec.Target,
		CacheFrom:  spec.CacheFrom,
		NoCache:    spec.NoCache,
		Remove:     true,
		Version:    build.BuilderBuildKit,
	})
	if err != nil {
		return runtime.ImageRef{}, fmt.Errorf("ImageBuild: %w", err)
	}

	if err := streamBuildOutput(ctx, res.Body, events); err != nil {
		return runtime.ImageRef{}, err
	}

	// Resolve the built image's digest via inspect for the returned ref.
	inspectRes, err := r.api.ImageInspect(ctx, spec.Tag)
	if err != nil {
		return runtime.ImageRef{}, fmt.Errorf("ImageInspect %s: %w", spec.Tag, err)
	}
	emitBuildEvent(events, runtime.BuildEvent{
		Kind:   runtime.BuildEventCompleted,
		Digest: inspectRes.ID,
	})
	return runtime.ImageRef{ID: inspectRes.ID, Tags: inspectRes.RepoTags}, nil
}

// streamBuildOutput parses the JSON-line stream from ImageBuild,
// emitting BuildEvents and returning a non-nil error if the daemon
// reports a build failure. Closes body before returning.
//
// Two response shapes are handled:
//
//   - Classic-builder-style records with `stream` (log lines) and
//     `status` (layer/pull progress) fields. Pre-BuildKit format.
//     Kept as a fallback — modern dockerd never emits these under
//     BuildKit, but harmless if a future daemon falls back.
//
//   - BuildKit records of the form `{"id":"moby.buildkit.trace",
//     "aux":"<base64-protobuf>"}` for per-step progress. The aux
//     payload is decoded by buildkitTraceDecoder via protowire — no
//     dependency on github.com/moby/buildkit. See buildkit_trace.go
//     for the subset of the StatusResponse schema we parse.
func streamBuildOutput(ctx context.Context, body io.ReadCloser, events chan<- runtime.BuildEvent) error {
	defer body.Close()

	type buildMsg struct {
		Stream      string          `json:"stream,omitempty"`
		Status      string          `json:"status,omitempty"`
		ID          string          `json:"id,omitempty"`
		Aux         json.RawMessage `json:"aux,omitempty"`
		ErrorDetail *struct {
			Message string `json:"message"`
		} `json:"errorDetail,omitempty"`
		Error string `json:"error,omitempty"`
	}

	trace := newBuildkitTraceDecoder()
	dec := json.NewDecoder(body)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var msg buildMsg
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode build output: %w", err)
		}
		if msg.ErrorDetail != nil {
			return fmt.Errorf("build failed: %s", msg.ErrorDetail.Message)
		}
		if msg.Error != "" {
			return fmt.Errorf("build failed: %s", msg.Error)
		}
		if line := strings.TrimRight(msg.Stream, "\n"); line != "" {
			emitBuildEvent(events, runtime.BuildEvent{
				Kind:    runtime.BuildEventLog,
				Message: line,
			})
		}
		if msg.Status != "" {
			emitBuildEvent(events, runtime.BuildEvent{
				Kind:    runtime.BuildEventLayer,
				Message: msg.Status,
			})
		}
		if msg.ID == "moby.buildkit.trace" && len(msg.Aux) > 0 {
			trace.handleAux(msg.Aux, events)
		}
	}
}

// tarDirectory writes the contents of dir (recursively) into w as a
// non-gzipped tar archive. Symlinks are preserved as tar TypeSymlink
// entries with their original target text; the daemon-side BuildKit
// frontend handles the resolution.
//
// Previously this passed an empty link argument to tar.FileInfoHeader
// for symlinks, producing tar entries with TypeSymlink + empty
// Linkname. Some downstream tar readers reject those as malformed and
// abort the build mid-stream — common in compose-primary contexts
// containing node_modules/.bin/* or similar bin-symlinks.
func tarDirectory(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Convert to forward slashes for cross-platform tar compatibility.
		rel = filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		var link string
		isSymlink := info.Mode()&os.ModeSymlink != 0
		if isSymlink {
			link, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("tar header for %s: %w", path, err)
		}
		hdr.Name = rel
		if d.IsDir() {
			hdr.Name = rel + "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() || isSymlink {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
}

// prePullBaseImages reads the Dockerfile at contextPath/dockerfile,
// extracts FROM image references (resolving simple $ARG substitutions
// against ARG defaults in the file and the spec's BuildArgs
// overrides), and pulls each that isn't already in the local image
// store. Best-effort: any pull error is silently ignored — the
// subsequent ImageBuild surfaces a clearer "failed to resolve" error
// if the image really can't be obtained, and images that exist only
// locally (e.g. dc-go-base-*) correctly skip the pull attempt because
// ImageInspect finds them.
func prePullBaseImages(ctx context.Context, r *Runtime, contextPath, dockerfile string, buildArgs map[string]string, events chan<- runtime.BuildEvent) {
	df, err := os.ReadFile(filepath.Join(contextPath, dockerfile))
	if err != nil {
		return
	}
	for _, ref := range extractBaseImages(string(df), buildArgs) {
		if _, err := r.api.ImageInspect(ctx, ref); err == nil {
			continue
		}
		_, _ = r.PullImage(ctx, ref, events)
	}
}

// extractBaseImages does a naive line-based parse of Dockerfile
// content, returning the de-duplicated list of resolved FROM image
// references. Supports:
//
//   - Literal FROM lines (`FROM alpine:3.20`, with optional
//     `AS <stage>` and `--platform=...` flags).
//   - $VAR / ${VAR} substitution against the file's `ARG VAR=default`
//     lines, with overrides from buildArgs taking precedence.
//
// FROMs whose ref still contains an unresolved `$` after substitution
// are dropped — we'd rather miss a pre-pull (and let BuildKit surface
// its native error) than guess wrong. Stage references (`FROM
// previous-stage AS …`) are also dropped since they're not image
// refs; we detect them by skipping refs that match a prior AS name.
func extractBaseImages(content string, buildArgs map[string]string) []string {
	args := map[string]string{}
	for k, v := range buildArgs {
		args[k] = v
	}
	stages := map[string]bool{}
	seen := map[string]bool{}
	var out []string

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kw := strings.ToUpper(fields[0])
		switch kw {
		case "ARG":
			rest := strings.TrimSpace(line[len("ARG"):])
			if eq := strings.IndexByte(rest, '='); eq > 0 {
				name := strings.TrimSpace(rest[:eq])
				val := strings.TrimSpace(rest[eq+1:])
				// Only set if not already overridden by buildArgs.
				if _, override := args[name]; !override {
					args[name] = val
				}
			}
		case "FROM":
			rest := strings.TrimSpace(line[len("FROM"):])
			// Drop leading flags (--platform=..., --link, ...).
			for strings.HasPrefix(rest, "--") {
				if sp := strings.IndexByte(rest, ' '); sp > 0 {
					rest = strings.TrimSpace(rest[sp+1:])
				} else {
					rest = ""
				}
			}
			// Trim "AS <stage>" suffix, recording the stage name.
			ref := rest
			if idx := indexFoldWord(rest, "AS"); idx > 0 {
				ref = strings.TrimSpace(rest[:idx])
				stageName := strings.TrimSpace(rest[idx+len("AS"):])
				if stageName != "" {
					stages[stageName] = true
				}
			}
			ref = substituteArgs(ref, args)
			if ref == "" || strings.Contains(ref, "$") {
				continue
			}
			if stages[ref] {
				continue
			}
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

// indexFoldWord returns the byte index of the first occurrence of
// `word` in s as a standalone whitespace-delimited token, matching
// case-insensitively. Returns -1 if not found.
func indexFoldWord(s, word string) int {
	wlen := len(word)
	for i := 0; i+wlen <= len(s); i++ {
		if !strings.EqualFold(s[i:i+wlen], word) {
			continue
		}
		// Must be at start or preceded by whitespace.
		if i > 0 && s[i-1] != ' ' && s[i-1] != '\t' {
			continue
		}
		// Must be at end or followed by whitespace.
		if i+wlen < len(s) && s[i+wlen] != ' ' && s[i+wlen] != '\t' {
			continue
		}
		return i
	}
	return -1
}

// substituteArgs replaces $VAR and ${VAR} occurrences in ref with the
// corresponding value from args. Leaves unknown vars unsubstituted so
// the caller can detect and skip refs with unresolved vars.
func substituteArgs(ref string, args map[string]string) string {
	var b strings.Builder
	i := 0
	for i < len(ref) {
		if ref[i] != '$' {
			b.WriteByte(ref[i])
			i++
			continue
		}
		// $ at end of string — keep as-is.
		if i+1 >= len(ref) {
			b.WriteByte('$')
			i++
			continue
		}
		var name string
		var consumed int
		if ref[i+1] == '{' {
			end := strings.IndexByte(ref[i+2:], '}')
			if end < 0 {
				b.WriteByte('$')
				i++
				continue
			}
			name = ref[i+2 : i+2+end]
			consumed = 2 + end + 1
		} else {
			j := i + 1
			for j < len(ref) && (isIdentByte(ref[j])) {
				j++
			}
			name = ref[i+1 : j]
			consumed = j - i
		}
		if val, ok := args[name]; ok {
			b.WriteString(val)
		} else {
			b.WriteString(ref[i : i+consumed])
		}
		i += consumed
	}
	return b.String()
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
