//go:build windows

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// dialUserScopedUnix is deliberately unavailable on Windows.  The product
// endpoint is a named pipe there; silently falling back to TCP would remove
// the user-scoped authentication boundary.
func dialUserScopedUnix(_ context.Context, _ string) (io.ReadWriteCloser, error) {
	return nil, errors.New("Unix sockets are unavailable on Windows")
}

// dialUserNamedPipe opens the canonical \\.\pipe\... path emitted by
// DefaultRuntimeEndpoint.  The runtime creates the pipe with a DACL limited
// to the current user; the client never exposes a network listener.
func dialUserNamedPipe(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := make(chan struct {
		file *os.File
		err  error
	}, 1)
	go func() {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		result <- struct {
			file *os.File
			err  error
		}{file: file, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case opened := <-result:
		if opened.err != nil {
			return nil, fmt.Errorf("open runtime named pipe %q: %w", path, opened.err)
		}
		return opened.file, nil
	}
}
