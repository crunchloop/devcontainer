package docker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/crunchloop/devcontainer/runtime"
)

// buildkitTraceDecoder decodes the `moby.buildkit.trace` aux records
// dockerd emits when building under BuildKit. The aux payload is a
// base64-encoded `controlapi.StatusResponse` protobuf:
//
//	message StatusResponse {
//	  repeated Vertex      vertexes = 1;
//	  repeated VertexStatus statuses = 2;
//	  repeated VertexLog   logs     = 3;
//	}
//	message Vertex {
//	  string digest = 1; repeated string inputs = 2; string name = 3;
//	  bool cached = 4; Timestamp started = 5; Timestamp completed = 6;
//	  string error = 7; ...
//	}
//	message VertexLog {
//	  string vertex = 1; Timestamp timestamp = 2; int64 stream = 3;
//	  bytes msg = 4;
//	}
//
// We parse the wire format directly via protowire — buildkit's own Go
// types live in github.com/moby/buildkit, which would pull ~250
// transitive modules (containerd, k8s, sigstore, AWS/Azure SDKs). The
// fields we care about (name, cached, started, completed, error, log
// msg) are stable and a hand-roll is ~150 LOC.
//
// State is tracked across StatusResponse updates: BuildKit re-sends the
// same vertex digest with incremental field updates. We dedupe by
// emitting BuildLayerEvent only on start- and complete-transitions per
// digest; VertexLog records always emit BuildLogEvent.
type buildkitTraceDecoder struct {
	seenStart    map[string]bool
	seenComplete map[string]bool
}

func newBuildkitTraceDecoder() *buildkitTraceDecoder {
	return &buildkitTraceDecoder{
		seenStart:    map[string]bool{},
		seenComplete: map[string]bool{},
	}
}

// handleAux base64-decodes and parses a moby.buildkit.trace aux
// payload. Best-effort: malformed records are silently ignored — the
// build's authoritative success/failure is reported via the outer
// JSON-line stream's `error`/`errorDetail` fields, not via aux.
func (d *buildkitTraceDecoder) handleAux(aux json.RawMessage, events chan<- runtime.BuildEvent) {
	var b64 string
	if err := json.Unmarshal(aux, &b64); err != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return
	}
	d.decodeStatus(raw, events)
}

func (d *buildkitTraceDecoder) decodeStatus(buf []byte, events chan<- runtime.BuildEvent) {
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return
		}
		buf = buf[n:]
		switch {
		case num == 1 && typ == protowire.BytesType: // Vertex
			v, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			d.decodeVertex(v, events)
			buf = buf[m:]
		case num == 3 && typ == protowire.BytesType: // VertexLog
			v, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			d.decodeLog(v, events)
			buf = buf[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, buf)
			if m < 0 {
				return
			}
			buf = buf[m:]
		}
	}
}

func (d *buildkitTraceDecoder) decodeVertex(buf []byte, events chan<- runtime.BuildEvent) {
	var (
		digest, name, vErr        string
		cached                    bool
		hasStarted, hasCompleted  bool
	)
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return
		}
		buf = buf[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			s, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			digest = string(s)
			buf = buf[m:]
		case num == 3 && typ == protowire.BytesType:
			s, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			name = string(s)
			buf = buf[m:]
		case num == 4 && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(buf)
			if m < 0 {
				return
			}
			cached = v != 0
			buf = buf[m:]
		case num == 5 && typ == protowire.BytesType:
			_, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			hasStarted = true
			buf = buf[m:]
		case num == 6 && typ == protowire.BytesType:
			_, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			hasCompleted = true
			buf = buf[m:]
		case num == 7 && typ == protowire.BytesType:
			s, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			vErr = string(s)
			buf = buf[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, buf)
			if m < 0 {
				return
			}
			buf = buf[m:]
		}
	}
	if digest == "" || name == "" {
		return
	}
	if hasStarted && !d.seenStart[digest] {
		d.seenStart[digest] = true
		emitBuildEvent(events, runtime.BuildEvent{
			Kind:    runtime.BuildEventLayer,
			Message: fmt.Sprintf("START %s", name),
			LayerID: digest,
		})
	}
	if hasCompleted && !d.seenComplete[digest] {
		d.seenComplete[digest] = true
		status := "DONE"
		switch {
		case vErr != "":
			status = "ERROR: " + vErr
		case cached:
			status = "CACHED"
		}
		emitBuildEvent(events, runtime.BuildEvent{
			Kind:    runtime.BuildEventLayer,
			Message: fmt.Sprintf("%s %s", status, name),
			LayerID: digest,
		})
	}
}

func (d *buildkitTraceDecoder) decodeLog(buf []byte, events chan<- runtime.BuildEvent) {
	var msg []byte
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return
		}
		buf = buf[n:]
		switch {
		case num == 4 && typ == protowire.BytesType:
			s, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return
			}
			msg = s
			buf = buf[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, buf)
			if m < 0 {
				return
			}
			buf = buf[m:]
		}
	}
	if len(msg) == 0 {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(msg), "\n"), "\n") {
		if line == "" {
			continue
		}
		emitBuildEvent(events, runtime.BuildEvent{
			Kind:    runtime.BuildEventLog,
			Message: line,
		})
	}
}
