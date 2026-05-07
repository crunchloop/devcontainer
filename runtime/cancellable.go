package runtime

import (
	"context"
	"io"
)

// CancellableCopy copies src to dst until the copy completes or ctx is
// cancelled. On cancellation, src.Close() is called to break out of any
// blocking read, and ctx.Err() is returned.
//
// This is the standard escape hatch for streaming Docker SDK calls
// (image pull, image build, follow logs) where ctx-driven cancellation
// is not honored at the socket-read boundary. Without it, ctx.Cancel()
// can hang for tens of seconds while the daemon stops sending bytes.
func CancellableCopy(ctx context.Context, dst io.Writer, src io.ReadCloser) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(dst, src)
		done <- err
	}()
	select {
	case err := <-done:
		_ = src.Close()
		return err
	case <-ctx.Done():
		_ = src.Close()
		<-done // drain; the goroutine returns once Close unblocks the read
		return ctx.Err()
	}
}
