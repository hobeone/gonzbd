package app

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// LoggingOptions configures structured logging behavior.
type LoggingOptions struct {
	// Level is the minimum log level (Debug, Info, Warn, Error).
	Level slog.Level
	// LogFile is the path to the log file. Empty means stderr only.
	LogFile string
	// AddSource adds file:line annotations to each log record.
	AddSource bool
}

// Setup returns a configured *slog.Logger that writes to stderr and
// optionally to a log file. The logger is also installed as slog.Default
// so that package-level slog.* calls work.
//
// The returned io.Closer is the log file handle (nil if LogFile is empty).
// Caller must call Close on it during shutdown to flush and close the file.
//
// If LogFile is non-empty, Setup creates the parent directory with mode
// 0o750 and opens the file with O_APPEND|O_CREATE|O_WRONLY, mode 0o640.
func Setup(opts LoggingOptions) (*slog.Logger, io.Closer, error) {
	var writers []io.Writer
	var closer io.Closer

	// Always write to stderr.
	writers = append(writers, os.Stderr)

	// Optionally write to file.
	if opts.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(opts.LogFile), 0o750); err != nil {
			return nil, nil, fmt.Errorf("create log directory: %w", err)
		}

		f, err := os.OpenFile(opts.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) //nolint:gosec // G302: log file mode is intentionally group-readable
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", opts.LogFile, err)
		}
		writers = append(writers, f)
		closer = f
	}

	// Create a MultiWriter that fans out to all configured sinks.
	mw := io.MultiWriter(writers...)

	// Create the logger with TextHandler.
	logger := slog.New(slog.NewTextHandler(mw, &slog.HandlerOptions{
		Level:     opts.Level,
		AddSource: opts.AddSource,
	}))

	// Install as the default logger for package-level slog.* calls.
	slog.SetDefault(logger)

	return logger, closer, nil
}
