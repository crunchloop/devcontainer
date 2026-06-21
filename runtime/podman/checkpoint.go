package podman

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/crunchloop/devcontainer/runtime"
)

// Checkpoint exports a running container to a self-contained archive via
// the libpod checkpoint endpoint with export=true (the response body is
// the tar archive). Verified against podman 5.4:
//
//	POST /libpod/containers/{id}/checkpoint?export=true&tcpestablished=&leaverunning=
func (r *Runtime) Checkpoint(ctx context.Context, id string, spec runtime.CheckpointSpec) (runtime.CheckpointRef, error) {
	q := url.Values{}
	q.Set("export", "true")
	q.Set("tcpestablished", strconv.FormatBool(spec.TCPEstablished))
	q.Set("leaverunning", strconv.FormatBool(!spec.StopAfter))

	resp, err := r.lp.do(ctx, http.MethodPost, "/containers/"+id+"/checkpoint", q, nil, "")
	if err != nil {
		return runtime.CheckpointRef{}, &runtime.CheckpointFailedError{ID: id, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return runtime.CheckpointRef{}, &runtime.CheckpointFailedError{ID: id, Stderr: errorBody(resp)}
	}

	f, err := os.Create(spec.ArchivePath)
	if err != nil {
		return runtime.CheckpointRef{}, &runtime.CheckpointFailedError{ID: id, Err: err}
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return runtime.CheckpointRef{}, &runtime.CheckpointFailedError{ID: id, Err: copyErr}
	}
	if closeErr != nil {
		return runtime.CheckpointRef{}, &runtime.CheckpointFailedError{ID: id, Err: closeErr}
	}
	return runtime.CheckpointRef{ArchivePath: spec.ArchivePath, Size: n}, nil
}

// restoreReport is the libpod restore response shape, e.g.
// {"Id":"<64hex>","runtime_restore_duration":0,"criu_statistics":null}.
type restoreReport struct {
	ID string `json:"Id"`
}

// Restore re-creates and resumes a container from a checkpoint archive,
// uploading the archive in the request body. Verified against podman 5.4:
//
//	POST /libpod/containers/import/restore?import=true&tcpestablished=&name=
//	  (body: the tar archive; "import" is the literal path segment)
func (r *Runtime) Restore(ctx context.Context, spec runtime.RestoreSpec) (*runtime.Container, error) {
	f, err := os.Open(spec.ArchivePath)
	if err != nil {
		return nil, &runtime.RestoreFailedError{ArchivePath: spec.ArchivePath, Err: err}
	}
	defer f.Close()

	q := url.Values{}
	q.Set("import", "true")
	q.Set("tcpestablished", strconv.FormatBool(spec.TCPEstablished))
	if spec.Name != "" {
		q.Set("name", spec.Name)
	}

	resp, err := r.lp.do(ctx, http.MethodPost, "/containers/import/restore", q, f, "application/x-tar")
	if err != nil {
		return nil, &runtime.RestoreFailedError{ArchivePath: spec.ArchivePath, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &runtime.RestoreFailedError{ArchivePath: spec.ArchivePath, Stderr: errorBody(resp)}
	}

	var rep restoreReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, &runtime.RestoreFailedError{ArchivePath: spec.ArchivePath, Err: err}
	}
	return &runtime.Container{ID: rep.ID, Name: spec.Name, State: runtime.StateRunning}, nil
}
