package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mortezaPRK/netns-mcast-relay/internal/netns"
)

const maxMutatingBodyBytes int64 = 4 << 10

var (
	errInvalidJSONBody        = errors.New("invalid JSON body")
	errPathRequired           = errors.New("path is required")
	errPathMustBeAbsolute     = errors.New("path must be absolute")
	errRequestBodyTooLarge    = errors.New("request body too large")
	errInvalidNamespaceFormat = errors.New("namespace path must be /proc/<pid>/ns/net format")
	errInvalidContainerID     = errors.New("container ID must be 64 lowercase hexadecimal characters")
)

var namespacePathPattern = regexp.MustCompile(`^/proc/[0-9]+/ns/net$`)
var containerIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// NamespaceController is the runtime control surface needed by the control API.
type NamespaceController interface {
	AddNamespace(ctx context.Context, path string) error
	RemoveNamespace(ctx context.Context, path string) error
	ActiveNamespaces() []string
}

// Server is the Unix-socket control API.
type Server struct {
	server     *http.Server
	listener   net.Listener
	socketPath string
	logger     *slog.Logger
	timeout    time.Duration
}

// NewUnixServer configures and binds the control API. Call Run to serve it.
func NewUnixServer(
	logger *slog.Logger,
	controller NamespaceController,
	socketPath string,
	timeout time.Duration,
	socketMode os.FileMode,
	socketGroup string,
) (*Server, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("control socket path is empty")
	}
	if !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf("control socket path must be absolute: %s", socketPath)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("control timeout must be positive")
	}
	if socketMode == 0 || socketMode.Perm() != socketMode {
		return nil, fmt.Errorf("invalid control socket mode: %04o", socketMode)
	}
	socketGID, err := resolveSocketGroup(socketGroup)
	if err != nil {
		return nil, err
	}

	socketDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create control socket directory: %w", err)
	}

	if err := removeStaleSocket(socketPath); err != nil {
		return nil, err
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to bind control socket %s: %w", socketPath, err)
	}
	if socketGID >= 0 {
		if err := os.Chown(socketPath, -1, socketGID); err != nil {
			_ = listener.Close()
			_ = os.Remove(socketPath)
			return nil, fmt.Errorf("failed to set control socket group %s: %w", socketGroup, err)
		}
	}
	if err := os.Chmod(socketPath, socketMode); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("failed to set control socket permissions: %w", err)
	}

	mux := http.NewServeMux()
	var containerMu sync.Mutex
	containerNamespaces := make(map[string]string)
	server := &http.Server{
		Handler:      withRequestTimeout(mux, timeout),
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
	}

	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /v1/namespaces", func(w http.ResponseWriter, _ *http.Request) {
		namespaces := controller.ActiveNamespaces()
		writeJSON(w, http.StatusOK, map[string]any{
			"namespaces": namespaces,
			"count":      len(namespaces),
		})
	})

	mux.HandleFunc("POST /v1/namespaces/add", func(w http.ResponseWriter, req *http.Request) {
		path, err := extractNamespacePath(w, req, maxMutatingBodyBytes)
		if err != nil {
			writeRequestError(w, err)
			return
		}

		path, err = canonicalizeNamespacePath(path)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		if err := controller.AddNamespace(req.Context(), path); err != nil {
			logger.Error("Control add namespace failed", "path", path, "error", err)
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		logger.Info("Control add namespace accepted", "path", path)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":  true,
			"applied":   false,
			"operation": "add",
			"path":      path,
			"mode":      "intent",
		})
	})

	mux.HandleFunc("POST /v1/namespaces/remove", func(w http.ResponseWriter, req *http.Request) {
		path, err := extractNamespacePath(w, req, maxMutatingBodyBytes)
		if err != nil {
			writeRequestError(w, err)
			return
		}

		path, err = canonicalizeNamespacePath(path)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		if err := controller.RemoveNamespace(req.Context(), path); err != nil {
			logger.Error("Control remove namespace failed", "path", path, "error", err)
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		logger.Info("Control remove namespace accepted", "path", path)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":  true,
			"applied":   false,
			"operation": "remove",
			"path":      path,
			"mode":      "intent",
		})
	})

	mux.HandleFunc("PUT /v1/containers/{id}/namespace", func(w http.ResponseWriter, req *http.Request) {
		containerID := req.PathValue("id")
		if !containerIDPattern.MatchString(containerID) {
			writeJSONError(w, http.StatusBadRequest, errInvalidContainerID.Error())
			return
		}

		path, err := extractNamespacePath(w, req, maxMutatingBodyBytes)
		if err != nil {
			writeRequestError(w, err)
			return
		}
		path, err = canonicalizeNamespacePath(path)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		containerMu.Lock()
		defer containerMu.Unlock()

		if oldPath, ok := containerNamespaces[containerID]; ok && oldPath != path {
			if err := controller.RemoveNamespace(req.Context(), oldPath); err != nil {
				logger.Error("Control container namespace replacement removal failed", "container_id", containerID, "path", oldPath, "error", err)
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if err := controller.AddNamespace(req.Context(), path); err != nil {
			logger.Error("Control container namespace upsert failed", "container_id", containerID, "path", path, "error", err)
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		containerNamespaces[containerID] = path

		logger.Info("Control container namespace upsert accepted", "container_id", containerID, "path", path)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":     true,
			"applied":      false,
			"operation":    "upsert",
			"container_id": containerID,
			"path":         path,
			"mode":         "intent",
		})
	})

	mux.HandleFunc("DELETE /v1/containers/{id}/namespace", func(w http.ResponseWriter, req *http.Request) {
		containerID := req.PathValue("id")
		if !containerIDPattern.MatchString(containerID) {
			writeJSONError(w, http.StatusBadRequest, errInvalidContainerID.Error())
			return
		}

		containerMu.Lock()
		defer containerMu.Unlock()

		path, ok := containerNamespaces[containerID]
		if ok {
			if err := controller.RemoveNamespace(req.Context(), path); err != nil {
				logger.Error("Control container namespace removal failed", "container_id", containerID, "path", path, "error", err)
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			delete(containerNamespaces, containerID)
		}

		logger.Info("Control container namespace removal accepted", "container_id", containerID)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":     true,
			"applied":      false,
			"operation":    "remove",
			"container_id": containerID,
			"mode":         "intent",
		})
	})

	return &Server{server: server, listener: listener, socketPath: socketPath, logger: logger, timeout: timeout}, nil
}

func resolveSocketGroup(groupName string) (int, error) {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		return -1, nil
	}
	group, err := user.LookupGroup(groupName)
	if err != nil {
		return -1, fmt.Errorf("failed to resolve control socket group %s: %w", groupName, err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid < 0 {
		return -1, fmt.Errorf("invalid GID %q for control socket group %s", group.Gid, groupName)
	}
	return gid, nil
}

// Run serves requests until ctx is cancelled or the server fails.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn("Control server shutdown error", "socket", s.socketPath, "error", err)
		}
	}()

	s.logger.Info("Control server listening", "socket", s.socketPath)
	err := s.server.Serve(s.listener)
	if removeErr := os.Remove(s.socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		s.logger.Warn("Failed to remove control socket", "socket", s.socketPath, "error", removeErr)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func withRequestTimeout(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

func removeStaleSocket(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if (info.Mode() & os.ModeSocket) == 0 {
			return fmt.Errorf("control socket path exists and is not a socket: %s", path)
		}

		active, probeErr := isActiveUnixSocket(path)
		if probeErr != nil {
			return probeErr
		}
		if active {
			return fmt.Errorf("control socket already in use: %s", path)
		}

		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to remove stale control socket: %w", err)
		}
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("failed to stat control socket path: %w", err)
}

func isActiveUnixSocket(path string) (bool, error) {
	dialer := net.Dialer{Timeout: 200 * time.Millisecond}
	conn, err := dialer.Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	return false, fmt.Errorf("failed to probe control socket %s: %w", path, err)
}

func extractNamespacePath(w http.ResponseWriter, req *http.Request, maxBodyBytes int64) (string, error) {
	req.Body = http.MaxBytesReader(w, req.Body, maxBodyBytes)

	if !strings.Contains(req.Header.Get("Content-Type"), "application/json") {
		return "", errInvalidJSONBody
	}
	var payload struct {
		Path string `json:"path"`
	}
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return "", errRequestBodyTooLarge
		}
		return "", errInvalidJSONBody
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return "", errRequestBodyTooLarge
		}
		return "", errInvalidJSONBody
	}
	if strings.TrimSpace(payload.Path) == "" {
		return "", errPathRequired
	}
	return strings.TrimSpace(payload.Path), nil
}

func canonicalizeNamespacePath(path string) (string, error) {
	cleaned, err := netns.CanonicalizePath(path)
	if err != nil {
		if strings.Contains(err.Error(), "required") {
			return "", errPathRequired
		}
		return "", errPathMustBeAbsolute
	}

	if !namespacePathPattern.MatchString(cleaned) {
		return "", errInvalidNamespaceFormat
	}

	return cleaned, nil
}

func writeRequestError(w http.ResponseWriter, err error) {
	if errors.Is(err, errRequestBodyTooLarge) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	writeJSONError(w, http.StatusBadRequest, err.Error())
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
