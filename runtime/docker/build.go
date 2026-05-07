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

	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

// BuildImage builds an image from a build context directory and tags
// it. Implements runtime.Runtime.BuildImage.
//
// Streaming progress messages are mapped onto the events channel as
// runtime.BuildEvents (drop-on-full). Build failures surface as a
// non-nil error including any structured error returned by the daemon.
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
// non-gzipped tar archive. Symlinks are followed (their targets are
// included as regular files) — the build daemon doesn't need our
// engineering tmp dirs to preserve link semantics.
//
// Empty / unreadable files are best-effort logged via the returned
// error rather than silently dropped.
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
		hdr, err := tar.FileInfoHeader(info, "")
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
		if d.IsDir() || (info.Mode()&os.ModeSymlink != 0 && hdr.Typeflag != tar.TypeReg) {
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
