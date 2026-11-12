package migrations

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every migration file runs in one implicit transaction, so a failure rolls
// the whole file back and the runner can reset the version (ErrRolledBack).
// That holds only without explicit transaction control and without
// statements that cannot run in a transaction block
// (docs/deploy-prereqs-plan.md B7).
func TestMigrationsLint(t *testing.T) {
	t.Parallel()
	names, err := fs.Glob(files, "*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, names)
	for _, name := range names {
		body, err := files.ReadFile(name)
		require.NoError(t, err)
		require.NoError(t, lintMigration(string(body)), name)
	}
}

func TestLintMigration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
		// want is a part of the refusal, or "" when the file is allowed.
		want string
	}{
		{"plain statements", "CREATE TABLE a (id int);\nALTER TABLE a ADD COLUMN b int;", ""},
		{"DO block", "DO $$\nBEGIN\n  PERFORM 1;\nEND\n$$;\nCREATE TABLE a (id int);", ""},
		{"tagged dollar quote", "CREATE FUNCTION f() RETURNS int AS $body$ BEGIN; COMMIT; END $body$ LANGUAGE plpgsql;", ""},
		{"comment mentions", "-- CREATE INDEX CONCURRENTLY later; COMMIT\n/* BEGIN; VACUUM; /* nested */ COMMIT; */\nCREATE TABLE a (id int);", ""},
		{"string mentions", "INSERT INTO t (s) VALUES ('BEGIN; COMMIT; VACUUM'), (E'it\\'s; COMMIT');", ""},
		{"CASE ... END", "UPDATE t SET a = CASE WHEN b THEN 1 ELSE 2 END;", ""},
		{"marked single statement", NonTransactionalMarker + "\n-- why\nCREATE INDEX CONCURRENTLY ix_a_b ON a (b);\n", ""},
		{"marked single statement without semicolon", NonTransactionalMarker + "\nVACUUM ANALYZE a", ""},

		{"BEGIN", "BEGIN;\nCREATE TABLE a (id int);\nCOMMIT;", "transaction control"},
		{"COMMIT", "CREATE TABLE a (id int);\nCOMMIT;\nSELECT 1/0;", "transaction control"},
		{"START TRANSACTION", "START TRANSACTION ISOLATION LEVEL SERIALIZABLE;", "transaction control"},
		{"END", "CREATE TABLE a (id int);\nend;", "transaction control"},
		{"ROLLBACK", "ROLLBACK;", "transaction control"},
		{"SAVEPOINT", "SAVEPOINT s;", "transaction control"},
		{"RELEASE", "RELEASE SAVEPOINT s;", "transaction control"},
		{"PREPARE TRANSACTION", "PREPARE TRANSACTION 'x';", "transaction control"},
		{"transaction control in a marked file", NonTransactionalMarker + "\nCOMMIT;", "transaction control"},
		{"unmarked CREATE INDEX CONCURRENTLY", "CREATE INDEX CONCURRENTLY ix_a_b ON a (b);", "non-transactional"},
		{"unmarked unique index", "create unique index\n  concurrently ix_a_b ON a (b);", "non-transactional"},
		{"unmarked DROP INDEX CONCURRENTLY", "DROP INDEX CONCURRENTLY ix_a_b;", "non-transactional"},
		{"unmarked REINDEX CONCURRENTLY", "REINDEX TABLE CONCURRENTLY a;", "non-transactional"},
		{"unmarked VACUUM", "VACUUM a;", "non-transactional"},
		{"unmarked CREATE DATABASE", "CREATE DATABASE x;", "non-transactional"},
		{"unmarked ALTER SYSTEM", "ALTER SYSTEM SET work_mem = '64MB';", "non-transactional"},
		{"marker not on the first line", "-- header\n" + NonTransactionalMarker + "\nVACUUM a;", "non-transactional"},
		{"marked file with two statements", NonTransactionalMarker + "\nCREATE TABLE a (id int);\nCREATE INDEX CONCURRENTLY ix_a_id ON a (id);", "exactly one statement"},
		{"empty marked file", NonTransactionalMarker + "\n", "exactly one statement"},
		{"unterminated string", "INSERT INTO t VALUES ('x);", "unterminated"},
		{"unterminated dollar quote", "DO $$ BEGIN END;", "unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := lintMigration(tt.sql)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// TargetMinAppVersion is what the newest embedded migration leaves in
// schema_compat: the last UPDATE of min_app_version, or the baseline.
func TestTargetMinAppVersion(t *testing.T) {
	t.Parallel()
	names, err := fs.Glob(files, "*.up.sql")
	require.NoError(t, err)

	want := baselineVersion
	setBy := regexp.MustCompile(`(?is)^UPDATE\s+schema_compat\s+SET\s.*\bmin_app_version\s*=\s*'([^']*)'`)
	for _, name := range names { // fs.Glob sorts, and the names are zero-padded
		body, rerr := files.ReadFile(name)
		require.NoError(t, rerr)
		stmts, serr := splitStatements(string(body), true)
		require.NoError(t, serr, name)
		for _, s := range stmts {
			if m := setBy.FindStringSubmatch(s); m != nil {
				want = m[1]
			}
		}
	}
	_, err = ParseVersion(want)
	require.NoError(t, err)
	require.Equal(t, want, TargetMinAppVersion,
		"a migration raises schema_compat.min_app_version: raise migrations.TargetMinAppVersion with it")
}

var (
	// Statements that end, open, or split a transaction.
	txControl = regexp.MustCompile(`^(BEGIN|COMMIT|END|ROLLBACK|ABORT|SAVEPOINT|RELEASE|START TRANSACTION|PREPARE TRANSACTION)\b`)
	// Statements that PostgreSQL refuses inside a transaction block.
	nonTxStatement = regexp.MustCompile(`^(VACUUM|CREATE DATABASE|ALTER SYSTEM)\b|` +
		`^CREATE (UNIQUE )?INDEX CONCURRENTLY\b|^DROP INDEX CONCURRENTLY\b|^REINDEX\b.*\bCONCURRENTLY\b`)
	spaces = regexp.MustCompile(`\s+`)
)

// lintMigration refuses a migration file that would not run as one
// transaction.
func lintMigration(body string) error {
	stmts, err := splitStatements(body, false)
	if err != nil {
		return err
	}
	firstLine, _, _ := strings.Cut(body, "\n")
	marked := strings.TrimSpace(firstLine) == NonTransactionalMarker
	if marked && len(stmts) != 1 {
		return fmt.Errorf("a file marked %q must hold exactly one statement, it holds %d", NonTransactionalMarker, len(stmts))
	}
	for _, s := range stmts {
		norm := strings.ToUpper(spaces.ReplaceAllString(s, " "))
		if txControl.MatchString(norm) {
			return fmt.Errorf("explicit transaction control is not allowed (every file is one transaction): %q", s)
		}
		if nonTxStatement.MatchString(norm) && !marked {
			return fmt.Errorf("a non-transactional statement must be alone in a file whose first line is %q: %q",
				NonTransactionalMarker, s)
		}
	}
	return nil
}

var errUnterminated = errors.New("unterminated string, quoted identifier, comment, or dollar quote")

// splitStatements removes comments and dollar-quoted bodies from a SQL file
// and splits it at semicolons into trimmed, non-empty statements. String
// literals and quoted identifiers are kept when keepStrings is set, else
// they become a space, so their words are not taken for keywords.
//
// It does not understand SQL-standard function bodies (BEGIN ATOMIC ...
// END): their inner semicolons split the statement, and the lint refuses
// the trailing END. Write such functions with a dollar-quoted body.
func splitStatements(sql string, keepStrings bool) ([]string, error) {
	var stmts []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			stmts = append(stmts, s)
		}
		cur.Reset()
	}
	isIdent := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				i = len(sql)
			} else {
				i += end
			}
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			// Block comments nest in PostgreSQL.
			depth, j := 0, i
			for ; j < len(sql); j++ {
				if strings.HasPrefix(sql[j:], "/*") {
					depth++
					j++
				} else if strings.HasPrefix(sql[j:], "*/") {
					depth--
					j++
					if depth == 0 {
						break
					}
				}
			}
			if depth != 0 {
				return nil, errUnterminated
			}
			cur.WriteByte(' ')
			i = j + 1
		case c == '\'' || c == '"':
			// E'...' strings take backslash escapes; others double the quote.
			escapes := c == '\'' && i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !isIdent(sql[i-2]))
			j := i + 1
			for ; j < len(sql); j++ {
				if escapes && sql[j] == '\\' {
					j++
					continue
				}
				if sql[j] == c {
					if j+1 < len(sql) && sql[j+1] == c {
						j++
						continue
					}
					break
				}
			}
			if j >= len(sql) {
				return nil, errUnterminated
			}
			if keepStrings {
				cur.WriteString(sql[i : j+1])
			} else {
				cur.WriteByte(' ')
			}
			i = j + 1
		case c == '$' && (i == 0 || !isIdent(sql[i-1])):
			// $tag$ ... $tag$; a tag does not start with a digit ($1 is a
			// parameter).
			j := i + 1
			for j < len(sql) && isIdent(sql[j]) && (j > i+1 || sql[j] < '0' || sql[j] > '9') {
				j++
			}
			if j >= len(sql) || sql[j] != '$' {
				cur.WriteByte(c)
				i++
				continue
			}
			tag := sql[i : j+1]
			end := strings.Index(sql[j+1:], tag)
			if end < 0 {
				return nil, errUnterminated
			}
			cur.WriteByte(' ')
			i = j + 1 + end + len(tag)
		case c == ';':
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts, nil
}
