package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// TargetMinAppVersion is the min_app_version that the newest embedded
// migration leaves in schema_compat: the oldest release that works with the
// schema of this binary. `backend migrate target-compat` prints it, so the
// deploy tool can stop a running release below it before migrating
// (deploy plan §7.3 step 7). A contract migration that raises
// min_app_version raises this constant in the same change; a test compares
// them.
const TargetMinAppVersion = "1.0.0"

// baselineVersion is min_app_version of a database without schema_compat
// (releases before migration 0022).
const baselineVersion = "1.0.0"

// sqlstateUndefinedTable is PostgreSQL's undefined_table.
const sqlstateUndefinedTable = "42P01"

// Version is a release version X.Y.Z.
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion parses a release version "X.Y.Z" (no leading v, no
// pre-release or build suffix). Anything else, such as "dev" or a
// git describe string, is not a release.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q is not X.Y.Z", s)
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') || strings.TrimLeft(p, "0123456789") != "" {
			return Version{}, fmt.Errorf("version %q is not X.Y.Z", s)
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("version %q is not X.Y.Z", s)
		}
		n[i] = v
	}
	return Version{Major: n[0], Minor: n[1], Patch: n[2]}, nil
}

// Compare returns -1, 0, or +1 when v is lower than, equal to, or higher
// than w.
func (v Version) Compare(w Version) int {
	for _, d := range [...]int{v.Major - w.Major, v.Minor - w.Minor, v.Patch - w.Patch} {
		switch {
		case d < 0:
			return -1
		case d > 0:
			return 1
		}
	}
	return 0
}

// String formats v as X.Y.Z.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Status is the schema state that decides which releases may run.
type Status struct {
	// SchemaVersion is the applied migration version; 0 when none is.
	SchemaVersion uint
	// Dirty is golang-migrate's dirty flag: a migration failed part-way.
	Dirty bool
	// MinAppVersion is the oldest release that works with the schema.
	MinAppVersion string
}

// Compatible reports whether a binary of the given version may run against
// the schema. A build that is not a release (dev, a git describe string) is
// always compatible: it is a developer's build.
func (s Status) Compatible(version string) bool {
	v, err := ParseVersion(version)
	if err != nil {
		return true
	}
	minV, err := ParseVersion(s.MinAppVersion)
	if err != nil {
		// The table's CHECK makes this impossible; refuse rather than guess.
		return false
	}
	return v.Compare(minV) >= 0
}

// Compat reads the schema version, the dirty flag, and min_app_version. It
// only reads: unlike golang-migrate, it creates no schema_migrations table
// and takes no migration lock, so it can run beside `migrate up`. It opens
// its own connection (db.OpenMigrationDB) and closes it before it returns.
func Compat(ctx context.Context, databaseURL string) (Status, error) {
	sqlDB, err := db.OpenMigrationDB(databaseURL)
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = sqlDB.Close() }()

	var st Status
	// golang-migrate's table holds at most one row (its SetVersion
	// truncates it first).
	var version int64
	err = sqlDB.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &st.Dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows), isUndefinedTable(err):
		// No migration applied.
	case err != nil:
		return Status{}, fmt.Errorf("read schema version: %w", err)
	case version > 0:
		st.SchemaVersion = uint(version)
	}

	err = sqlDB.QueryRowContext(ctx, `SELECT min_app_version FROM schema_compat WHERE id = 1`).Scan(&st.MinAppVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows), isUndefinedTable(err):
		st.MinAppVersion = baselineVersion
	case err != nil:
		return Status{}, fmt.Errorf("read min_app_version: %w", err)
	}
	return st, nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable
}
