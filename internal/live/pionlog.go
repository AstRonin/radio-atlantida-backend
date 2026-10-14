package live

import (
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// pionLogs routes pion's logs into the JSON log (component webrtc). pion's
// debug and info output is per packet or per candidate and is dropped; its
// warnings are mostly the noise of a closing session ("stream is already
// closed") and go to DEBUG; its errors are kept at WARN.
type pionLogs struct{ log *slog.Logger }

func newPionLogs(log *slog.Logger) *pionLogs { return &pionLogs{log: log} }

// NewLogger implements logging.LoggerFactory.
func (f *pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{log: f.log.With("pion", scope)}
}

type pionLogger struct{ log *slog.Logger }

func (pionLogger) Trace(string)          {}
func (pionLogger) Tracef(string, ...any) {}
func (pionLogger) Debug(string)          {}
func (pionLogger) Debugf(string, ...any) {}
func (pionLogger) Info(string)           {}
func (pionLogger) Infof(string, ...any)  {}

func (l pionLogger) Warn(msg string)                  { l.log.Debug(msg) }
func (l pionLogger) Warnf(format string, args ...any) { l.log.Debug(fmt.Sprintf(format, args...)) }

// Errors of pion are WARN here: they concern one session (a failed
// handshake, a closed peer), which ends with its own log line; an ERROR
// would raise an alert popup in the panel for each.
func (l pionLogger) Error(msg string)                  { l.log.Warn(msg) }
func (l pionLogger) Errorf(format string, args ...any) { l.log.Warn(fmt.Sprintf(format, args...)) }
