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
//
//   - BuildKit records of the form `{"id":"moby.buildkit.trace",
//     "aux":"<base64-protobuf>"}` for per-step progress and
//     `{"id":"moby.image.id","aux":{"ID":"sha256:..."}}` for the
//     final image. The aux protobuf is buildkit's `SolveStatus` —
//     decoding requires the buildkit module. We intentionally don't
//     pull that dep in: per-step progress events are silently dropped
//     under BuildKit; BuildStart / BuildCompleted (emitted by the
//     caller and at the end of BuildImage) still fire correctly, and
//     errors still propagate via `errorDetail` / `error` fields.
//     A future PR can revisit if vertex-level progress is needed.
func streamBuildOutput(ctx context.Context, body io.ReadCloser, events chan<- runtime.BuildEvent) error {
	defer body.Close()

	type buildMsg struct {
		Stream      string `json:"stream,omitempty"`
		Status      string `json:"status,omitempty"`
		ErrorDetail *struct {
			Message string `json:"message"`
		} `json:"errorDetail,omitempty"`
		Error string `json:"error,omitempty"`
	}

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
