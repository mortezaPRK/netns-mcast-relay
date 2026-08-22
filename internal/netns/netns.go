package netns

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// CanonicalizePath trims, cleans, and validates an absolute namespace path.
func CanonicalizePath(path string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return "", errors.New("namespace path is required")
	}
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("namespace path must be absolute: %s", path)
	}
	return cleaned, nil
}

// NSHandle wraps a network namespace handle
type NSHandle struct {
	handle netns.NsHandle
	path   string
}

// Open opens a network namespace by file path
func Open(path string) (*NSHandle, error) {
	// Open the namespace file descriptor
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open namespace %s: %w", path, err)
	}

	handle := netns.NsHandle(fd)

	return &NSHandle{
		handle: handle,
		path:   path,
	}, nil
}

// Close closes the namespace handle
func (h *NSHandle) Close() error {
	return h.handle.Close()
}

// Do executes a function within the namespace
// The function is executed on a locked OS thread to ensure namespace isolation
func (h *NSHandle) Do(fn func() error) (retErr error) {
	// Lock the OS thread to prevent the Go scheduler from moving the goroutine
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Get the current namespace to restore later
	currentNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current namespace: %w", err)
	}
	defer func() {
		if closeErr := currentNs.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("failed to close saved namespace handle: %w", closeErr))
		}
	}()

	// Switch to the target namespace
	if err := netns.Set(h.handle); err != nil {
		return fmt.Errorf("failed to set namespace: %w", err)
	}

	// Ensure we restore the original namespace
	defer func() {
		if setErr := netns.Set(currentNs); setErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("failed to restore namespace: %w", setErr))
		}
	}()

	// Execute the function
	retErr = fn()
	return retErr
}

// Path returns the filesystem path of the namespace
func (h *NSHandle) Path() string {
	return h.path
}
