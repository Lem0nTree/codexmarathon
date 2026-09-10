//go:build !windows

package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func listenControllerControlEndpoint(endpoint, stateDir string) (net.Listener, string, error) {
	if err := ValidateControllerControlEndpoint(endpoint, stateDir); err != nil {
		return nil, "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, "", fmt.Errorf("parse controller endpoint: %w", err)
	}
	if strings.ToLower(parsed.Scheme) != "unix" {
		return nil, "", fmt.Errorf("%w: only Unix sockets are available on this platform", ErrControlUnsupportedOS)
	}
	path := filepath.Clean(filepath.FromSlash(parsed.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", fmt.Errorf("create controller socket directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil && !isPermissionMetadataError(err) {
		return nil, "", fmt.Errorf("tighten controller socket directory: %w", err)
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return nil, "", fmt.Errorf("%w: existing endpoint is not a Unix socket", ErrUnsafeControlEndpoint)
		}
		probe, probeErr := net.DialTimeout("unix", path, 150*time.Millisecond)
		if probeErr == nil {
			_ = probe.Close()
			return nil, "", ErrControlAlreadyRunning
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, "", fmt.Errorf("remove stale controller endpoint: %w", removeErr)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, "", fmt.Errorf("inspect controller endpoint: %w", statErr)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", fmt.Errorf("listen on controller endpoint %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, "", fmt.Errorf("tighten controller endpoint permissions: %w", err)
	}
	return listener, path, nil
}

func removeControllerControlEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || strings.ToLower(parsed.Scheme) != "unix" {
		return nil
	}
	path := filepath.Clean(filepath.FromSlash(parsed.Path))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: refusing to remove non-socket endpoint", ErrUnsafeControlEndpoint)
	}
	return os.Remove(path)
}
