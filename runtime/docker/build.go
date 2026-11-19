package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

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

	// Bound and read the Dockerfile before anything else: a refused
	// path fails here, before the context is tarred or the daemon is
	// called, and nothing below reads it from the host again.
	dockerfile, dockerfileBody, injected, err := locateDockerfile(spec.ContextPath, spec.Dockerfile, spec.WorkspaceRoot)
	if err != nil {
		return runtime.ImageRef{}, err
	}

	// Stream the context directory as a tar over an io.Pipe so we
	// don't buffer it all in memory.
	pr, pw := io.Pipe()
	go func() {
		err := tarDirectory(spec.ContextPath, injected, pw)
		_ = pw.CloseWithError(err)
	}()

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
	prePullBaseImages(ctx, r, dockerfileBody, spec.Args, events)

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

// tarEpoch is the canonical normalized mtime stamped into every tar
// header by tarDirectory. Using unix epoch (rather than time.Time{},
// which clamps to epoch anyway in USTAR) makes intent explicit and
// avoids any reader special-casing the year-1 sentinel.
var tarEpoch = time.Unix(0, 0)

// tarFile is a regular file appended to a build-context tar after the
// directory walk.
type tarFile struct {
	Name string
	Body []byte
}

// maxInjectedDockerfileSize caps a Dockerfile read from the host,
// which bounds what can be appended to the context tar from outside
// the context. Real Dockerfiles are a few KiB.
const maxInjectedDockerfileSize = 1 << 20

// locateDockerfile maps a BuildSpec.Dockerfile onto the name the
// daemon should open inside the context tar (tarName) and the host
// copy's contents (body, for pre-pulling base images; nil when the
// host copy of an in-context Dockerfile can't be read, leaving the
// daemon to report it). dockerfile is relative to contextPath (empty
// means "Dockerfile") or an absolute host path.
//
// A path inside the context becomes its context-relative name, as
// before. One outside the context can't be reached through the tar,
// so it is returned as injected, to be appended under a reserved
// name, matching `docker build -f <outside> <context>` (which the
// reference devcontainer CLI runs). The name carries a content hash
// rather than the docker CLI's random suffix so the tar stays
// reproducible.
//
// The path comes from an untrusted devcontainer.json, and the daemon
// echoes a bad Dockerfile's first line in its parse error, so every
// host read is bounded first: the file must resolve, after symlinks,
// inside the context or workspaceRoot, and one outside the context
// only inside workspaceRoot. It must also be a regular file of at
// most maxInjectedDockerfileSize. A refused path yields a
// *runtime.DockerfileRefusedError and nothing is read.
func locateDockerfile(contextPath, dockerfile, workspaceRoot string) (tarName string, body []byte, injected *tarFile, err error) {
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	absCtx, err := filepath.Abs(contextPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("BuildImage: context path %s: %w", contextPath, err)
	}
	hostPath := dockerfile
	if !filepath.IsAbs(hostPath) {
		hostPath = filepath.Join(absCtx, hostPath)
	}

	if rel, ok := lexicallyWithin(absCtx, hostPath); ok {
		// The daemon opens it from the tar. The host copy is only
		// read for pre-pulling, and a failure to read it is left for
		// the daemon to report, as before; escaping the bound is not.
		real, err := resolveWithin(dockerfile, hostPath, absCtx, workspaceRoot)
		var refused *runtime.DockerfileRefusedError
		if errors.As(err, &refused) {
			return "", nil, nil, err
		}
		if err == nil {
			body, _ = readDockerfile(dockerfile, real)
		}
		return filepath.ToSlash(rel), body, nil, nil
	}

	real, err := resolveWithin(dockerfile, hostPath, workspaceRoot)
	if err != nil {
		return "", nil, nil, err
	}
	body, err = readDockerfile(dockerfile, real)
	if err != nil {
		return "", nil, nil, err
	}
	sum := sha256.Sum256(body)
	name := ".dockerfile." + hex.EncodeToString(sum[:])[:12]
	return name, body, &tarFile{Name: name, Body: body}, nil
}

// lexicallyWithin reports whether path is root or below it, without
// touching the filesystem, and returns path relative to root.
func lexicallyWithin(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	return rel, true
}

// resolveWithin resolves hostPath's symlinks and returns the real
// path if it lies inside one of roots (each also resolved; empty
// roots are skipped). Otherwise it returns a
// *runtime.DockerfileRefusedError naming configured. A path that
// can't be resolved is refused too unless it lies lexically inside a
// root, so a missing file outside the bound can't be told apart from
// an existing one.
func resolveWithin(configured, hostPath string, roots ...string) (string, error) {
	var bounds []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		bounds = append(bounds, root)
	}
	if len(bounds) == 0 {
		return "", &runtime.DockerfileRefusedError{Path: configured, Reason: "outside the build context, and no workspace root is set"}
	}
	outside := &runtime.DockerfileRefusedError{Path: configured, Reason: "resolves outside " + strings.Join(bounds, " and ")}

	real, err := filepath.EvalSymlinks(hostPath)
	if err != nil {
		for _, root := range bounds {
			if absRoot, aerr := filepath.Abs(root); aerr == nil {
				if _, ok := lexicallyWithin(absRoot, hostPath); ok {
					return "", fmt.Errorf("BuildImage: dockerfile %s: %w", configured, err)
				}
			}
		}
		return "", outside
	}
	for _, root := range bounds {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if _, ok := lexicallyWithin(realRoot, real); ok {
			return real, nil
		}
	}
	return "", outside
}

// readDockerfile reads real, a path resolveWithin returned, as a
// regular file of at most maxInjectedDockerfileSize. The file is
// opened non-blocking and re-checked through its descriptor, so a
// FIFO or device can't stall the build and a file swapped after the
// check isn't read.
func readDockerfile(configured, real string) ([]byte, error) {
	notRegular := &runtime.DockerfileRefusedError{Path: configured, Reason: "not a regular file"}
	tooLarge := &runtime.DockerfileRefusedError{Path: configured, Reason: fmt.Sprintf("larger than %d bytes", maxInjectedDockerfileSize)}

	fi, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("BuildImage: dockerfile %s: %w", configured, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegular
	}
	if fi.Size() > maxInjectedDockerfileSize {
		return nil, tooLarge
	}
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("BuildImage: dockerfile %s: %w", configured, err)
	}
	defer func() { _ = f.Close() }()
	ofi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("BuildImage: dockerfile %s: %w", configured, err)
	}
	if !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return nil, &runtime.DockerfileRefusedError{Path: configured, Reason: "changed while being checked"}
	}
	body, err := io.ReadAll(io.LimitReader(f, maxInjectedDockerfileSize+1))
	if err != nil {
		return nil, fmt.Errorf("BuildImage: dockerfile %s: %w", configured, err)
	}
	if len(body) > maxInjectedDockerfileSize {
		return nil, tooLarge
	}
	return body, nil
}

// tarDirectory writes the contents of dir (recursively) into w as a
// non-gzipped tar archive, followed by extra when non-nil. Symlinks are preserved as tar TypeSymlink
// entries with their original target text; the daemon-side BuildKit
// frontend handles the resolution.
//
// Previously this passed an empty link argument to tar.FileInfoHeader
// for symlinks, producing tar entries with TypeSymlink + empty
// Linkname. Some downstream tar readers reject those as malformed and
// abort the build mid-stream — common in compose-primary contexts
// containing node_modules/.bin/* or similar bin-symlinks.
func tarDirectory(dir string, extra *tarFile, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
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
		// Normalize metadata so the tar stream — and therefore
		// BuildKit's COPY vertex digest — is reproducible across
		// invocations and machines. Wall-clock mtimes from
		// os.WriteFile (e.g. useruid's synthesized context) and
		// host-specific uid/gid would otherwise perturb the digest of
		// byte-identical content, causing cache misses and forcing
		// re-extraction of downstream image layers. BuildKit hashes
		// content for cache purposes, so erasing these fields doesn't
		// lose information it relies on.
		hdr.ModTime = tarEpoch
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""

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
	if err != nil || extra == nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     extra.Name,
		Mode:     0o644,
		Size:     int64(len(extra.Body)),
		ModTime:  tarEpoch,
	}); err != nil {
		return err
	}
	_, err = tw.Write(extra.Body)
	return err
}

// prePullBaseImages takes the Dockerfile contents locateDockerfile read,
// extracts its frontend image (a `# syntax=` directive or the
// BUILDKIT_SYNTAX build arg) and FROM image references (resolving
// simple $ARG substitutions against ARG defaults in the file and the
// spec's BuildArgs overrides), and pulls each that isn't already in
// the local image store. The frontend needs it as much as FROM
// images: BuildKit resolves it from the registry before parsing the
// Dockerfile, under the same session requirement. Best-effort: any
// pull error is silently ignored — the subsequent ImageBuild surfaces
// a clearer "failed to resolve" error if the image really can't be
// obtained, and images that exist only locally (e.g. dc-go-base-*)
// correctly skip the pull attempt because ImageInspect finds them.
//
// BuildKit releases since v0.31 (Docker Engine 29.6) fall back to
// anonymous registry access when no session is attached, so this only
// changes the outcome on older engines; there it is what makes public
// images resolvable at all.
func prePullBaseImages(ctx context.Context, r *Runtime, df []byte, buildArgs map[string]string, events chan<- runtime.BuildEvent) {
	if df == nil {
		return
	}
	refs := extractBaseImages(string(df), buildArgs)
	if frontend := syntaxFrontend(df, buildArgs); frontend != "" {
		refs = append([]string{frontend}, refs...)
	}
	for _, ref := range refs {
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

// syntaxFrontend returns the frontend image BuildKit will run for a
// Dockerfile, or "" for the built-in one. The BUILDKIT_SYNTAX build
// arg wins over the file; otherwise the `syntax` parser directive is
// read the way BuildKit's parser.DetectSyntax reads it:
//
//   - after an optional UTF-8 BOM and a leading `#!` line;
//   - only from the leading run of directive lines, each `#` (or `//`)
//     at column 0 followed by `key = value` with key one of syntax,
//     escape, check (case-insensitive) — the first other line ends the
//     run;
//   - the image is the value's first word (the rest is frontend
//     arguments);
//   - failing both comment forms, a file that is a JSON object with a
//     string "syntax" member names the image with the whole value (the
//     input format of custom frontends that take JSON).
func syntaxFrontend(content []byte, buildArgs map[string]string) string {
	if v, ok := buildArgs["BUILDKIT_SYNTAX"]; ok {
		return firstWord(v)
	}
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	if first, rest, _ := bytes.Cut(content, []byte("\n")); bytes.HasPrefix(first, []byte("#!")) {
		content = rest
	}
	for _, comment := range []string{"#", "//"} {
		if v, ok := directive(content, comment, "syntax"); ok {
			return firstWord(v)
		}
	}
	var doc map[string]any
	if json.Unmarshal(content, &doc) == nil {
		if v, ok := doc["syntax"].(string); ok {
			return v
		}
	}
	return ""
}

// directiveLine matches the body of a parser directive line.
var directiveLine = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)

// directive returns the value of the parser directive key in content's
// leading directive lines written with the given comment prefix.
func directive(content []byte, comment, key string) (string, bool) {
	for _, line := range strings.Split(string(content), "\n") {
		body, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), comment)
		if !ok {
			return "", false
		}
		m := directiveLine.FindStringSubmatch(strings.TrimLeftFunc(body, unicode.IsSpace))
		if m == nil {
			return "", false
		}
		switch name := strings.ToLower(m[1]); name {
		case key:
			return m[2], true
		case "syntax", "escape", "check":
		default:
			return "", false
		}
	}
	return "", false
}

func firstWord(s string) string {
	w, _, _ := strings.Cut(strings.TrimSpace(s), " ")
	return w
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
