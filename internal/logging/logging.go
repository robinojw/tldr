// Package logging provides structured stderr logging for tldr.
// All log output goes to stderr to avoid corrupting stdio MCP JSON-RPC traffic.
//
// Use SetGlobalLevel to change the level for all loggers (including those
// already created via New). Individual loggers can still override with
// SetLevel.
package logging

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
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
	default:
		return "UNKNOWN"
	}
}

// globalLevel is the process-wide minimum log level. Loggers that have not
// had SetLevel called explicitly will use this value.
var globalLevel atomic.Int32

func init() {
	globalLevel.Store(int32(LevelInfo))
}

// SetGlobalLevel sets the minimum log level for all loggers that have not
// overridden their level with SetLevel.
func SetGlobalLevel(level Level) {
	globalLevel.Store(int32(level))
}

// Logger provides structured logging to stderr.
type Logger struct {
	mu            sync.Mutex
	out           io.Writer
	level         Level
	levelOverride bool // true if SetLevel was called explicitly
	prefix        string
}

var defaultLogger = &Logger{
	out:   os.Stderr,
	level: LevelInfo,
}

// Default returns the default logger.
func Default() *Logger {
	return defaultLogger
}

// New creates a new logger with the given prefix.
// The logger inherits the global log level until SetLevel is called.
func New(prefix string) *Logger {
	return &Logger{
		out:    os.Stderr,
		level:  LevelInfo,
		prefix: prefix,
	}
}

// SetLevel sets the minimum log level for this logger, overriding the
// global level.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
	l.levelOverride = true
}

// SetOutput sets the log output writer.
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = w
}

// effectiveLevel returns the level to use: the logger's own override, or
// the global level.
func (l *Logger) effectiveLevel() Level {
	if l.levelOverride {
		return l.level
	}
	return Level(globalLevel.Load())
}

func (l *Logger) log(level Level, format string, args ...interface{}) {
	if level < l.effectiveLevel() {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	prefix := ""
	if l.prefix != "" {
		prefix = "[" + l.prefix + "] "
	}
	fmt.Fprintf(l.out, "%s %s %s%s\n", ts, level, prefix, msg)
}

// Debug logs a debug message.
func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(LevelDebug, format, args...)
}

// Info logs an info message.
func (l *Logger) Info(format string, args ...interface{}) {
	l.log(LevelInfo, format, args...)
}

// Warn logs a warning message.
func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(LevelWarn, format, args...)
}

// Error logs an error message.
func (l *Logger) Error(format string, args ...interface{}) {
	l.log(LevelError, format, args...)
}
