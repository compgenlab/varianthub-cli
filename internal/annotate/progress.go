package annotate

import (
	"context"
	"io"
	"log"
)

// Logger emits human-readable progress to stderr for `varhub annotate -v`. It is
// nil-safe: a nil *Logger silently drops every message, so callers can hold a nil
// logger in the non-verbose case without guarding each call. The logger is carried
// through the pipeline in the context (WithLogger / LoggerFrom) so threading it
// does not churn the many function signatures.
type Logger struct{ l *log.Logger }

// NewLogger returns a Logger writing to w (typically os.Stderr). Pass a nil writer
// to get a no-op logger.
func NewLogger(w io.Writer) *Logger {
	if w == nil {
		return nil
	}
	return &Logger{l: log.New(w, "varhub: ", log.Ltime)}
}

// Logf prints one progress line. Safe to call on a nil *Logger (no-op).
func (lg *Logger) Logf(format string, a ...any) {
	if lg == nil {
		return
	}
	lg.l.Printf(format, a...)
}

type loggerKey struct{}

// WithLogger returns a context carrying lg (which may be nil).
func WithLogger(ctx context.Context, lg *Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, lg)
}

// LoggerFrom returns the context's logger, or nil when none is set. The nil is
// itself a usable (no-op) *Logger.
func LoggerFrom(ctx context.Context) *Logger {
	lg, _ := ctx.Value(loggerKey{}).(*Logger)
	return lg
}

// Count formats an integer with thousands separators for log lines (exported for
// callers outside this package, e.g. the CLI's locus-path counts).
func Count(n int) string { return commaCount(n) }

// commaCount formats an integer with thousands separators, so a running variant
// count reads as "1,250,000" rather than "1250000".
func commaCount(n int) string {
	if n < 0 {
		return "-" + commaCount(-n)
	}
	if n < 1000 {
		return itoa(n)
	}
	return commaCount(n/1000) + "," + pad3(n%1000)
}

func pad3(n int) string {
	s := itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
