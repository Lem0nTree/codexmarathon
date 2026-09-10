//go:build windows

package app

import (
	"fmt"
	"net"
)

// Named-pipe serving is intentionally left behind the same platform seam as
// the runtime connector.  The Rust runtime owns the protected Windows pipe;
// a future TUI/controller build can add a DACL-backed implementation here
// without weakening the Linux Unix-socket boundary.
func listenControllerControlEndpoint(endpoint, stateDir string) (net.Listener, string, error) {
	return nil, "", fmt.Errorf("%w: controller named-pipe serving is not implemented", ErrControlUnsupportedOS)
}

func removeControllerControlEndpoint(endpoint string) error { return nil }
