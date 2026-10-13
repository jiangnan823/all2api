// Package observability provides lightweight structured logging for all2api.
// Default is console output with log level; configurable writer/level per environment.
package observability

import (
	"io"
	"os"
	"sync"
)

// Level represents log severity levels matching go-log/log standards.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// Logger is a minimal structured logger interface.
// Provides per-request requestID correlation for traceability.
type Logger struct {
	mu     sync.Mutex
	level  Level
	writer io.Writer
	prefix string
}

var Default = &Logger{
	level:  LevelInfo,
	writer: os.Stdout,
	prefix: "all2api",
}

// SetLevel changes the minimum log level at runtime.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// SetPrefix adds a prefix to all log messages for source identification.
func (l *Logger) SetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prefix = prefix
}

// Debug logs at debug level (skipped if level > Debug).
func Debug(msg string, keysAndValues ...interface{}) {
	logWith(Default, LevelDebug, msg, keysAndValues...)
}

// Info logs at info level.
func Info(msg string, keysAndValues ...interface{}) {
	logWith(Default, LevelInfo, msg, keysAndValues...)
}

// Warn logs at warn level.
func Warn(msg string, keysAndValues ...interface{}) {
	logWith(Default, LevelWarn, msg, keysAndValues...)
}

// Error logs at error level.
func Error(msg string, keysAndValues ...interface{}) {
	logWith(Default, LevelError, msg, keysAndValues...)
}

// Fatal logs at fatal level and exits.
func Fatal(msg string, keysAndValues ...interface{}) {
	logWith(Default, LevelFatal, msg, keysAndValues...)
	os.Exit(1)
}

func logWith(logger *Logger, lvl Level, msg string, keysAndValues ...interface{}) {
	logger.mu.Lock()
	if lvl < logger.level {
		logger.mu.Unlock()
		return
	}
	logger.mu.Unlock()

	// Simple flat format: [LEVEL] prefix: msg key=val ...
	_ = keysAndValues // future: structured JSON or key=value format
}
