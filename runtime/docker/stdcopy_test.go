package docker

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(stream byte, payload string) []byte {
	header := make([]byte, headerSize)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, []byte(payload)...)
}

func TestStdcopy_Demux(t *testing.T) {
	src := bytes.NewBuffer(nil)
	src.Write(frame(streamStdout, "hello "))
	src.Write(frame(streamStderr, "err1"))
	src.Write(frame(streamStdout, "world"))
	src.Write(frame(streamStderr, " err2"))

	var outBuf, errBuf bytes.Buffer
	if err := stdcopy(&outBuf, &errBuf, src); err != nil {
		t.Fatalf("stdcopy: %v", err)
	}
	if outBuf.String() != "hello world" {
		t.Errorf("stdout = %q", outBuf.String())
	}
	if errBuf.String() != "err1 err2" {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestStdcopy_NilWriterDiscards(t *testing.T) {
	src := bytes.NewBuffer(nil)
	src.Write(frame(streamStdout, "kept"))
	src.Write(frame(streamStderr, "dropped"))

	var outBuf bytes.Buffer
	if err := stdcopy(&outBuf, nil, src); err != nil {
		t.Fatalf("stdcopy: %v", err)
	}
	if outBuf.String() != "kept" {
		t.Errorf("stdout = %q", outBuf.String())
	}
}

func TestStdcopy_TruncatedFrame(t *testing.T) {
	// Header says 100 bytes of payload, but only 10 are available.
	header := make([]byte, headerSize)
	header[0] = streamStdout
	binary.BigEndian.PutUint32(header[4:], 100)
	src := bytes.NewReader(append(header, []byte(strings.Repeat("x", 10))...))

	var outBuf bytes.Buffer
	if err := stdcopy(&outBuf, nil, src); err == nil {
		t.Error("expected error on truncated frame")
	}
}

func TestStdcopy_EmptyStream(t *testing.T) {
	if err := stdcopy(nil, nil, bytes.NewBuffer(nil)); err != nil {
		t.Errorf("expected nil error on empty stream, got %v", err)
	}
}
