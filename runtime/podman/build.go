package podman

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/crunchloop/devcontainer/runtime"
)

var imageIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// buildQuery maps a BuildSpec to libpod /build query params (verified
// against podman 5.4: dockerfile is a JSON array, t is the tag).
func buildQuery(spec runtime.BuildSpec) url.Values {
	q := url.Values{}
	df := "Dockerfile"
	if spec.Dockerfile != "" {
		df = filepath.Base(spec.Dockerfile)
	}
	b, _ := json.Marshal([]string{df})
	q.Set("dockerfile", string(b))
	if spec.Tag != "" {
		q.Set("t", spec.Tag)
	}
	if len(spec.Args) > 0 {
		ba, _ := json.Marshal(spec.Args)
		q.Set("buildargs", string(ba))
	}
	if spec.Target != "" {
		q.Set("target", spec.Target)
	}
	if spec.NoCache {
		q.Set("nocache", "1")
	}
	if spec.Platform != "" {
		q.Set("platform", spec.Platform)
	}
	return q
}

// tarDir streams a tar of dir's contents (rooted at dir) into w.
func tarDir(dir string, w *io.PipeWriter) {
	tw := tar.NewWriter(w)
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = w.CloseWithError(walkErr)
		return
	}
	if err := tw.Close(); err != nil {
		_ = w.CloseWithError(err)
		return
	}
	_ = w.Close()
}

// BuildImage builds an image with buildah via the libpod /build endpoint:
// streams the context as a tar request body, forwards the build log as
// BuildEventLog events, and returns the built image's reference.
func (r *Runtime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	if spec.ContextPath == "" {
		return runtime.ImageRef{}, fmt.Errorf("podman BuildImage: ContextPath is required")
	}

	pr, pw := io.Pipe()
	go tarDir(spec.ContextPath, pw)

	resp, err := r.lp.do(ctx, http.MethodPost, "/build", buildQuery(spec), pr, "application/x-tar")
	if err != nil {
		return runtime.ImageRef{}, fmt.Errorf("podman build: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return runtime.ImageRef{}, fmt.Errorf("podman build: %s", errorBody(resp))
	}

	id, err := parseBuildStream(resp.Body, events)
	if err != nil {
		return runtime.ImageRef{}, fmt.Errorf("podman build: %w", err)
	}
	if id == "" {
		return runtime.ImageRef{}, fmt.Errorf("podman build: no image id in build output")
	}
	ref := runtime.ImageRef{ID: id}
	if spec.Tag != "" {
		ref.Tags = []string{spec.Tag}
	}
	if events != nil {
		select {
		case events <- runtime.BuildEvent{Kind: runtime.BuildEventCompleted, Digest: id}:
		default:
		}
	}
	return ref, nil
}

type buildMsg struct {
	Stream string `json:"stream"`
	Error  string `json:"error"`
}

// parseBuildStream consumes the libpod build response (a stream of JSON
// objects), forwards {"stream":...} as BuildEventLog events, and returns
// the built image id — the last stream line that is a bare 64-hex digest
// (verified: podman emits the full image id as the final stream line).
func parseBuildStream(r io.Reader, events chan<- runtime.BuildEvent) (string, error) {
	dec := json.NewDecoder(r)
	var imageID string
	for {
		var m buildMsg
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return imageID, err
		}
		if m.Error != "" {
			return imageID, fmt.Errorf("%s", m.Error)
		}
		if m.Stream == "" {
			continue
		}
		if events != nil {
			select {
			case events <- runtime.BuildEvent{Kind: runtime.BuildEventLog, Message: m.Stream}:
			default:
			}
		}
		if t := strings.TrimSpace(m.Stream); imageIDRe.MatchString(t) {
			imageID = t
		}
	}
	return imageID, nil
}
