package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// libpodClient is a thin HTTP client for Podman's libpod REST API over a
// unix socket — the endpoints not covered by the docker-compatible API
// the embedded docker.Runtime uses (checkpoint/restore, buildah build).
//
// Deliberately dependency-free (stdlib net/http only): the official
// containers/podman/v5/pkg/bindings drags in the whole Podman module
// (cgo, gpgme, storage build tags, ~300 extra modules) — see
// design/podman-backend.md. This client is ~the same socket the moby
// client uses, so the backend has one transport and no CLI subprocess.
type libpodClient struct {
	hc      *http.Client
	baseURL string
}

// apiVersion is the version segment in the libpod path. Podman accepts a
// range down to its minimum; this is informational for the libpod API.
const apiVersion = "v5.0.0"

// newLibpodClient builds a client for the given Podman socket
// (e.g. "unix:///run/podman/podman.sock").
func newLibpodClient(socket string) *libpodClient {
	sockPath := socket
	for _, p := range []string{"unix://", "unix:"} {
		sockPath = strings.TrimPrefix(sockPath, p)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	return &libpodClient{
		hc:      &http.Client{Transport: tr},
		baseURL: "http://d/" + apiVersion + "/libpod",
	}
}

// do issues a request to the libpod API. The caller owns resp.Body.
func (c *libpodClient) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.hc.Do(req)
}

// ping reports whether the libpod API is reachable (GET /_ping → 2xx).
func (c *libpodClient) ping(ctx context.Context) bool {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil, "")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// apiError is the libpod error response shape
// (e.g. {"cause":"...","message":"...","response":500}).
type apiError struct {
	Cause    string `json:"cause"`
	Message  string `json:"message"`
	Response int    `json:"response"`
}

// errorBody reads an error response body into a short message.
func errorBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var e apiError
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return fmt.Sprintf("%s (http %d)", e.Message, resp.StatusCode)
	}
	return fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}
