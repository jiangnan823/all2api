// Package observability provides lightweight structured logging for all2api.
// Default is console output with log level; configurable writer/level per environment.
package observability

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
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
	ring   *Ring // optional panel-log sink; nil = disabled (see ring.go)
}

// Default prefixes every line with "all2api" on stdout. main.go also attaches
// the panel ring (AttachRing) so the wired lines land in GET /api/logs.
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
	ring := logger.ring
	logger.mu.Unlock()

	// Flat stdout line: [LEVEL] prefix: msg key=val ...
	line := formatLine(logger.prefix, lvl, msg, keysAndValues...)
	logger.mu.Lock()
	if logger.writer != nil {
		_, _ = io.WriteString(logger.writer, line)
	}
	logger.mu.Unlock()

	// Optional panel-log sink (M5a): append under the ring's own lock, never
	// the logger's — a slow consumer cannot stall other log calls, and the
	// ring holds only the formatted message (no writer I/O here).
	if ring != nil {
		ring.Append(Entry{TS: time.Now(), Level: lvl, Msg: formatMsg(msg, keysAndValues...)})
	}
}

// formatMsg renders msg plus the key=value tail (odd keysAndValues keep an
// explicit "EV" placeholder for the missing value instead of panicking).
func formatMsg(msg string, keysAndValues ...interface{}) string {
	if len(keysAndValues) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i < len(keysAndValues); i += 2 {
		b.WriteByte(' ')
		fmt.Fprint(&b, keysAndValues[i])
		b.WriteByte('=')
		if i+1 < len(keysAndValues) {
			fmt.Fprint(&b, keysAndValues[i+1])
		} else {
			b.WriteString("EV")
		}
	}
	return b.String()
}

// formatLine renders the full stdout decoration: "[LEVEL] prefix: msg k=v".
func formatLine(prefix string, lvl Level, msg string, keysAndValues ...interface{}) string {
	return "[" + lvl.String() + "] " + prefix + ": " + formatMsg(msg, keysAndValues...) + "\n"
}
