package docker

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/crunchloop/devcontainer/runtime"
)

// Wire-format helpers — build the minimal subset of StatusResponse we
// need to exercise the decoder, without depending on buildkit's
// generated Go types.

func appendString(buf []byte, fieldNum int, s string) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(fieldNum), protowire.BytesType)
	return protowire.AppendString(buf, s)
}

func appendBool(buf []byte, fieldNum int, b bool) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(fieldNum), protowire.VarintType)
	v := uint64(0)
	if b {
		v = 1
	}
	return protowire.AppendVarint(buf, v)
}

func appendSubmessage(buf []byte, fieldNum int, sub []byte) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(fieldNum), protowire.BytesType)
	return protowire.AppendBytes(buf, sub)
}

// vertex builds a Vertex submessage payload.
func vertex(digest, name string, cached, started, completed bool, vErr string) []byte {
	var b []byte
	if digest != "" {
		b = appendString(b, 1, digest)
	}
	if name != "" {
		b = appendString(b, 3, name)
	}
	if cached {
		b = appendBool(b, 4, true)
	}
	if started {
		// Empty Timestamp submessage — presence is what we test for.
		b = appendSubmessage(b, 5, nil)
	}
	if completed {
		b = appendSubmessage(b, 6, nil)
	}
	if vErr != "" {
		b = appendString(b, 7, vErr)
	}
	return b
}

func vertexLog(vertex string, msg []byte) []byte {
	var b []byte
	if vertex != "" {
		b = appendString(b, 1, vertex)
	}
	if len(msg) > 0 {
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendBytes(b, msg)
	}
	return b
}

func statusResponse(vertexes [][]byte, logs [][]byte) []byte {
	var b []byte
	for _, v := range vertexes {
		b = appendSubmessage(b, 1, v)
	}
	for _, l := range logs {
		b = appendSubmessage(b, 3, l)
	}
	return b
}

// drain collects events from a channel until it's empty (non-blocking).
func drain(ch chan runtime.BuildEvent) []runtime.BuildEvent {
	var out []runtime.BuildEvent
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func auxJSON(t *testing.T, payload []byte) json.RawMessage {
	t.Helper()
	b64 := base64.StdEncoding.EncodeToString(payload)
	raw, err := json.Marshal(b64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBuildkitTrace_VertexStartCompleteOnce(t *testing.T) {
	d := newBuildkitTraceDecoder()
	ch := make(chan runtime.BuildEvent, 16)

	// First update: vertex appears in "started" state.
	d.handleAux(auxJSON(t, statusResponse(
		[][]byte{vertex("sha256:aaa", "[1/2] RUN echo hi", false, true, false, "")},
		nil,
	)), ch)

	// Second update: same vertex, now also completed.
	d.handleAux(auxJSON(t, statusResponse(
		[][]byte{vertex("sha256:aaa", "[1/2] RUN echo hi", false, true, true, "")},
		nil,
	)), ch)

	// Third update: same vertex again — should emit nothing.
	d.handleAux(auxJSON(t, statusResponse(
		[][]byte{vertex("sha256:aaa", "[1/2] RUN echo hi", false, true, true, "")},
		nil,
	)), ch)

	got := drain(ch)
	if len(got) != 2 {
		t.Fatalf("event count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Kind != runtime.BuildEventLayer || got[0].Message != "START [1/2] RUN echo hi" || got[0].LayerID != "sha256:aaa" {
		t.Errorf("start event = %+v", got[0])
	}
	if got[1].Kind != runtime.BuildEventLayer || got[1].Message != "DONE [1/2] RUN echo hi" {
		t.Errorf("done event = %+v", got[1])
	}
}

func TestBuildkitTrace_CachedAndError(t *testing.T) {
	d := newBuildkitTraceDecoder()
	ch := make(chan runtime.BuildEvent, 16)

	d.handleAux(auxJSON(t, statusResponse(
		[][]byte{
			vertex("sha256:cached", "[cached] FROM alpine", true, true, true, ""),
			vertex("sha256:err", "[2/2] RUN false", false, true, true, "exit code 1"),
		},
		nil,
	)), ch)

	got := drain(ch)
	if len(got) != 4 {
		t.Fatalf("event count = %d, want 4: %+v", len(got), got)
	}
	// Order within a single StatusResponse: start then complete per vertex,
	// vertexes in field-order. We assert by digest+kind, not strict order.
	byMsg := map[string]runtime.BuildEvent{}
	for _, e := range got {
		byMsg[e.Message] = e
	}
	if e, ok := byMsg["CACHED [cached] FROM alpine"]; !ok || e.LayerID != "sha256:cached" {
		t.Errorf("missing CACHED event: %+v", byMsg)
	}
	if e, ok := byMsg["ERROR: exit code 1 [2/2] RUN false"]; !ok || e.LayerID != "sha256:err" {
		t.Errorf("missing ERROR event: %+v", byMsg)
	}
}

func TestBuildkitTrace_LogSplitsLines(t *testing.T) {
	d := newBuildkitTraceDecoder()
	ch := make(chan runtime.BuildEvent, 16)

	d.handleAux(auxJSON(t, statusResponse(
		nil,
		[][]byte{vertexLog("sha256:x", []byte("line one\nline two\n\nline three\n"))},
	)), ch)

	got := drain(ch)
	if len(got) != 3 {
		t.Fatalf("event count = %d, want 3: %+v", len(got), got)
	}
	for i, want := range []string{"line one", "line two", "line three"} {
		if got[i].Kind != runtime.BuildEventLog || got[i].Message != want {
			t.Errorf("event[%d] = %+v, want log %q", i, got[i], want)
		}
	}
}

func TestBuildkitTrace_MalformedSilentlyIgnored(t *testing.T) {
	d := newBuildkitTraceDecoder()
	ch := make(chan runtime.BuildEvent, 4)

	// Not valid base64.
	d.handleAux(json.RawMessage(`"!!!not-base64!!!"`), ch)
	// Not a JSON string.
	d.handleAux(json.RawMessage(`{"oops":true}`), ch)
	// Valid base64 but garbage protobuf — protowire's Consume returns
	// negative lengths and the decoder bails. No panic, no events.
	d.handleAux(auxJSON(t, []byte{0xff, 0xff, 0xff, 0xff}), ch)

	if got := drain(ch); len(got) != 0 {
		t.Errorf("expected no events on malformed input, got %+v", got)
	}
}

func TestBuildkitTrace_UnknownFieldsSkipped(t *testing.T) {
	// Forward-compat: a StatusResponse with an unknown high-numbered
	// field type should be skipped without breaking known field decoding.
	d := newBuildkitTraceDecoder()
	ch := make(chan runtime.BuildEvent, 4)

	payload := statusResponse(
		[][]byte{vertex("sha256:fwd", "[fwd] step", false, true, true, "")},
		nil,
	)
	// Append an unknown varint field (field number 99).
	payload = protowire.AppendTag(payload, 99, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 42)
	// And an unknown length-delimited field (100).
	payload = protowire.AppendTag(payload, 100, protowire.BytesType)
	payload = protowire.AppendString(payload, "future-field-value")

	d.handleAux(auxJSON(t, payload), ch)

	got := drain(ch)
	if len(got) != 2 {
		t.Errorf("expected 2 events despite unknown fields, got %d: %+v", len(got), got)
	}
}
