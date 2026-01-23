package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/spirilis/generic-go-mcp/config"
)

// Custom log level for TRACE (below DEBUG)
const LevelTrace = slog.Level(-8)

var (
	logger *slog.Logger
	level  slog.Level
)

// Initialize configures the global logger based on the provided config
func Initialize(cfg *config.LoggingConfig) {
	// Parse log level
	level = parseLevel(cfg.Level)

	// Create handler options
	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Rename custom TRACE level
			if a.Key == slog.LevelKey {
				lvl := a.Value.Any().(slog.Level)
				if lvl == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	}

	// Create handler based on format
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	logger = slog.New(handler)
}

// parseLevel converts a string level to slog.Level
func parseLevel(levelStr string) slog.Level {
	switch strings.ToLower(levelStr) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Trace logs a trace-level message
func Trace(msg string, args ...any) {
	if logger != nil {
		logger.Log(context.TODO(), LevelTrace, msg, args...)
	}
}

// Debug logs a debug-level message
func Debug(msg string, args ...any) {
	if logger != nil {
		logger.Debug(msg, args...)
	}
}

// Info logs an info-level message
func Info(msg string, args ...any) {
	if logger != nil {
		logger.Info(msg, args...)
	}
}

// Warn logs a warning-level message
func Warn(msg string, args ...any) {
	if logger != nil {
		logger.Warn(msg, args...)
	}
}

// Error logs an error-level message
func Error(msg string, args ...any) {
	if logger != nil {
		logger.Error(msg, args...)
	}
}

// IsTraceEnabled returns true if trace logging is enabled
func IsTraceEnabled() bool {
	return level <= LevelTrace
}

// IsDebugEnabled returns true if debug logging is enabled
func IsDebugEnabled() bool {
	return level <= slog.LevelDebug
}

// SanitizeHeaders returns a copy of headers with sensitive values redacted
func SanitizeHeaders(headers map[string][]string) map[string]string {
	sanitized := make(map[string]string)
	sensitiveHeaders := map[string]bool{
		"authorization": true,
		"cookie":        true,
		"set-cookie":    true,
		"x-api-key":     true,
		"x-auth-token":  true,
	}

	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		if sensitiveHeaders[lowerKey] {
			sanitized[key] = "[REDACTED]"
		} else if len(values) > 0 {
			sanitized[key] = values[0]
		}
	}

	return sanitized
}
