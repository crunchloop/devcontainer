package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCancellableCopy_Completes(t *testing.T) {
	src := io.NopCloser(strings.NewReader("hello"))
	var dst bytes.Buffer
	if err := CancellableCopy(context.Background(), &dst, src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dst.String() != "hello" {
		t.Errorf("dst = %q", dst.String())
	}
}

// blockingReader returns io.EOF only after Close is called.
type blockingReader struct {
	closed chan struct{}
	once   chan struct{}
}

func newBlockingReader() *blockingReader {
	return &blockingReader{
		closed: make(chan struct{}),
		once:   make(chan struct{}, 1),
	}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	select {
	case b.once <- struct{}{}:
		// Yield once with no data so io.Copy spins back into Read.
		return 0, nil
	default:
	}
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReader) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestCancellableCopy_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := newBlockingReader()
	var dst bytes.Buffer

	errCh := make(chan error, 1)
	go func() { errCh <- CancellableCopy(ctx, &dst, src) }()

	// Give the goroutine a moment to enter Read.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CancellableCopy did not return after ctx cancel")
	}
}
