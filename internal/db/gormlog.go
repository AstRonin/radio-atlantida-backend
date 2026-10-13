package db

import (
	"context"
	"log/slog"
	"time"

	gormlogger "gorm.io/gorm/logger"
)

// slowQueryThreshold is the duration above which a GORM query is logged.
const slowQueryThreshold = 500 * time.Millisecond

// gormLogger sends GORM's own messages and slow queries to slog. Failed
// queries are not logged here: GORM returns the error to the caller, which
// logs it once where it is handled.
type gormLogger struct {
	log *slog.Logger
}

func newGormLogger(log *slog.Logger) gormlogger.Interface {
	return gormLogger{log: log}
}

// LogMode is ignored: the level is controlled by the slog handler.
func (l gormLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l gormLogger) Info(ctx context.Context, msg string, args ...any) {
	l.log.InfoContext(ctx, msg, "args", args)
}

func (l gormLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.log.WarnContext(ctx, msg, "args", args)
}

func (l gormLogger) Error(ctx context.Context, msg string, args ...any) {
	l.log.ErrorContext(ctx, msg, "args", args)
}

// Trace logs a query that took longer than slowQueryThreshold. The SQL is
// logged with placeholders, never with the bound values (see ParamsFilter).
func (l gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), _ error) {
	elapsed := time.Since(begin)
	if elapsed < slowQueryThreshold {
		return
	}
	sql, rows := fc()
	l.log.WarnContext(ctx, "slow query", "duration_ms", elapsed.Milliseconds(), "rows", rows, "sql", sql)
}

// ParamsFilter drops the bound values, so Trace logs parameterized SQL.
func (gormLogger) ParamsFilter(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}
