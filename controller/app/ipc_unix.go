//go:build !windows

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// dialUserNamedPipe is kept behind the platform boundary so the common
// lifecycle code never attempts to emulate Windows IPC with a TCP socket.
func dialUserNamedPipe(_ context.Context, _ string) (io.ReadWriteCloser, error) {
	return nil, errors.New("Windows named pipes are unavailable on this platform")
}

func dialUserScopedUnix(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial runtime Unix socket %q: %w", path, err)
	}
	return conn, nil
}
