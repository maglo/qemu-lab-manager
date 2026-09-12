package serial

import (
	"io"
	"log/slog"
)

type slogLogger = slog.Logger

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
