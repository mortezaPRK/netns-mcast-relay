package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/mortezaPRK/netns-mcast-relay/internal/config"
	"github.com/mortezaPRK/netns-mcast-relay/internal/control"
	"github.com/mortezaPRK/netns-mcast-relay/internal/relay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse configuration
	cfg, err := config.Parse()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	// Setup structured logger
	logLevel := slog.LevelInfo
	if cfg.Verbose {
		logLevel = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	logger.Info("Starting netns-mcast-relay",
		"groups", len(cfg.Groups),
		"initial_namespaces", len(cfg.InitialNamespaces),
		"verbose", cfg.Verbose,
		"control_socket", cfg.ControlSocketPath,
		"control_socket_mode", cfg.ControlSocketMode,
		"control_socket_group", cfg.ControlSocketGroup)

	// Create relay instance
	r, err := relay.New(cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to create relay: %w", err)
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case sig := <-sigCh:
			logger.Info("Received signal, shutting down...", "signal", sig)
			cancel(nil)
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup

	// Start relay loop.
	wg.Go(func() {
		if err := r.Run(ctx); err != nil {
			logger.Error("Relay error", "error", err)
			cancel(fmt.Errorf("relay error: %w", err))
		}
	})

	if cfg.ControlSocketPath != "" {
		controlServer, err := control.NewUnixServer(
			logger,
			r,
			cfg.ControlSocketPath,
			cfg.ControlTimeout,
			cfg.ControlSocketMode,
			cfg.ControlSocketGroup,
		)
		if err != nil {
			cancel(err)
			wg.Wait()
			return fmt.Errorf("failed to start control server (%s): %w", cfg.ControlSocketPath, err)
		}

		wg.Go(func() {
			if err := controlServer.Run(ctx); err != nil {
				logger.Error("Control server error", "socket", cfg.ControlSocketPath, "error", err)
				cancel(fmt.Errorf("control server error: %w", err))
			}
		})
	}

	// Add initial namespaces if provided
	for _, nsPath := range cfg.InitialNamespaces {
		if err := r.AddNamespace(ctx, nsPath); err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				break
			}
			cancel(fmt.Errorf("failed to add initial namespace %s: %w", nsPath, err))
			break
		}
		logger.Info("Added initial namespace", "path", nsPath)
	}

	<-ctx.Done()
	wg.Wait()

	logger.Info("Shutdown complete")
	cause := context.Cause(ctx)
	if errors.Is(cause, context.Canceled) {
		return nil
	}
	return cause
}
