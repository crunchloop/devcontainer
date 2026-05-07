package docker

import (
	"encoding/binary"
	"errors"
	"io"
)

// Docker's hijacked exec stream multiplexes stdout/stderr into a framed
// protocol when no TTY is attached: each frame is an 8-byte header
// (byte 0 = stream id, bytes 4-7 = big-endian uint32 length) followed by
// the payload. With a TTY, no framing is used — the stream is opaque.
//
// stdcopy reads framed bytes from src and writes stdout frames to outW
// and stderr frames to errW. If outW or errW is nil, the corresponding
// stream is discarded. Returns io.EOF at clean end-of-stream.
const (
	streamStdin  byte = 0
	streamStdout byte = 1
	streamStderr byte = 2

	headerSize = 8
)

func stdcopy(outW, errW io.Writer, src io.Reader) error {
	header := make([]byte, headerSize)
	for {
		_, err := io.ReadFull(src, header)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		streamID := header[0]
		size := binary.BigEndian.Uint32(header[4:])
		if size == 0 {
			continue
		}
		var dst io.Writer
		switch streamID {
		case streamStdout:
			dst = outW
		case streamStderr:
			dst = errW
		case streamStdin:
			// Should never appear from the daemon, but ignore if it does.
			dst = nil
		default:
			// Unknown stream — discard to keep the framing aligned.
			dst = nil
		}
		if dst == nil {
			if _, err := io.CopyN(io.Discard, src, int64(size)); err != nil {
				return err
			}
			continue
		}
		if _, err := io.CopyN(dst, src, int64(size)); err != nil {
			return err
		}
	}
}
