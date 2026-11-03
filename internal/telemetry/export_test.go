package telemetry

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
)

// NewErrorHandlerForTest exposes the rate-limited error handler.
func NewErrorHandlerForTest(log *slog.Logger, interval time.Duration, now func() time.Time) otel.ErrorHandler {
	return newErrorHandler(log, interval, now)
}
