// Package logging contains shared logging helpers.
package logging

import (
	"context"
	"log/slog"
)

// DebugLazy invokes log only when debug logging is enabled. Expensive log
// attributes should be constructed inside log.
func DebugLazy(ctx context.Context, logger *slog.Logger, log func(context.Context, *slog.Logger)) {
	if logger == nil || !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	log(ctx, logger)
}
