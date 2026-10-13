// Package architecture checks the layering rules of the module (§5.1 of the
// design): SQL and database drivers stay in the repository layer, and the
// adapters (HTTP handlers, jobs) reach the repositories only through the
// domain packages.
package architecture

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sqlAllowed are the directories that may contain SQL and import the
// drivers: the repository layer, the connection and transaction helpers,
// the integration-test database, and the migration runner.
var sqlAllowed = []string{
	"internal/repository/",
	"internal/db/",
	"internal/dbtest/",
	"internal/migrations/",
}

// allowList holds deliberate exceptions: file (slash-separated, relative to
// the module root) → why it needs SQL outside the repository layer. Each
// entry also needs a comment at the code it excuses.
var allowList = map[string]string{}

// forbiddenImports are the driver packages (prefixes).
var forbiddenImports = []string{
	"gorm.io/",
	"github.com/jackc/pgx",
}

// sqlStatements match SQL in string literals. SQL keywords are written in
// upper case (CLAUDE.md), which keeps English messages such as "select a
// playlist" out of the match.
var sqlStatements = []*regexp.Regexp{
	regexp.MustCompile(`\bSELECT\b`),
	regexp.MustCompile(`\bINSERT\s+INTO\b`),
	regexp.MustCompile(`\bUPDATE\s+\S+\s+SET\b`),
	regexp.MustCompile(`\bDELETE\s+FROM\b`),
	regexp.MustCompile(`\bON\s+CONFLICT\b`),
}

// skipDirs are never scanned: no Go code of the service lives there.
var skipDirs = map[string]bool{".git": true, ".dev": true, "testdata": true, "vendor": true, "node_modules": true}

func TestSQLStaysInRepositoryLayer(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	var problems []string
	err := walkGoFiles(root, func(rel string, src []byte) error {
		if sqlPermitted(rel) {
			return nil
		}
		found, err := violations(rel, src)
		if err != nil {
			return err
		}
		problems = append(problems, found...)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, problems,
		"SQL and database drivers belong in internal/repository/<domain> (§5.1 of the design). "+
			"Move the code there, or add a commented entry to allowList:\n%s", strings.Join(problems, "\n"))
}

// adapterDirs are the driving adapters: they decode a request or start a
// run and call a domain service. Business rules and data access live in
// the domain packages, so an adapter never imports a repository, not even
// for a plain read. Tests are exempt: integration tests wire real
// repositories into the domain services they exercise.
var adapterDirs = []string{
	"internal/httpapi/",
	"internal/jobs/",
}

// repositoryImport is the import path prefix of the repository layer.
const repositoryImport = "github.com/AstRonin/radio-atlantida-backend/internal/repository"

func TestAdaptersUseDomainPackages(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	var problems []string
	err := walkGoFiles(root, func(rel string, src []byte) error {
		if !isAdapter(rel) {
			return nil
		}
		found, err := repositoryImports(rel, src)
		if err != nil {
			return err
		}
		problems = append(problems, found...)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, problems,
		"HTTP handlers and jobs call domain services, never repositories (§5.1 of the design). "+
			"Add the method to the domain package and call it from there:\n%s", strings.Join(problems, "\n"))
}

func TestRepositoryImports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"repository package", `import _ "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"`, []string{"x.go:3: import of github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"}},
		{"aliased", `import operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"`, []string{"x.go:3: import of github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"}},
		{"domain package", `import _ "github.com/AstRonin/radio-atlantida-backend/internal/operations"`, nil},
		{"similar prefix", `import _ "github.com/AstRonin/radio-atlantida-backend/internal/repositoryx"`, nil},
		{"mentioned in a comment", "// see internal/repository/jobs\nvar x = 1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := repositoryImports("x.go", []byte("package x\n\n"+tt.src+"\n"))

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsAdapter(t *testing.T) {
	t.Parallel()
	require.True(t, isAdapter("internal/httpapi/handlers/operations.go"))
	require.True(t, isAdapter("internal/jobs/scheduler.go"))
	require.False(t, isAdapter("internal/operations/operations.go"))
	require.False(t, isAdapter("cmd/backend/serve.go"))
	require.False(t, isAdapter("internal/jobsx/a.go"))
}

func TestAllowListEntriesExist(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	for file, reason := range allowList {
		require.NotEmpty(t, reason, "%s: allowList entries need a reason", file)
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		require.NoError(t, err, "%s: stale allowList entry", file)
	}
}

// TestViolations proves the check catches what it must, so it cannot pass by
// accident.
func TestViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"select", "var q = `SELECT id FROM t`", []string{"x.go:3: SQL statement (SELECT)"}},
		{"insert", `var q = "INSERT INTO t (v) VALUES ($1)"`, []string{"x.go:3: SQL statement (INSERT INTO)"}},
		{"update", "var q = `UPDATE operations SET v = 1`", []string{"x.go:3: SQL statement (UPDATE operations SET)"}},
		{"delete", "var q = `DELETE FROM t`", []string{"x.go:3: SQL statement (DELETE FROM)"}},
		{"upsert", "var q = `ON CONFLICT (id) DO NOTHING`", []string{"x.go:3: SQL statement (ON CONFLICT)"}},
		{"line inside a raw string", "var q = `\n\n\tDELETE FROM t`", []string{"x.go:5: SQL statement (DELETE FROM)"}},
		{"pgx import", `import _ "github.com/jackc/pgx/v5/pgxpool"`, []string{"x.go:3: import of github.com/jackc/pgx/v5/pgxpool"}},
		{"gorm import", `import _ "gorm.io/gorm"`, []string{"x.go:3: import of gorm.io/gorm"}},
		{"comments are ignored", "// SELECT id FROM t\nvar x = 1", nil},
		{"English messages are ignored", `var m = "select a playlist, then update the clock and delete from the grid"`, nil},
		{"other imports", `import _ "github.com/AstRonin/radio-atlantida-backend/internal/db"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := violations("x.go", []byte("package x\n\n"+tt.src+"\n"))

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSQLPermitted(t *testing.T) {
	t.Parallel()
	require.True(t, sqlPermitted("internal/repository/operations/operations.go"))
	require.True(t, sqlPermitted("internal/db/tx.go"))
	require.False(t, sqlPermitted("internal/operations/operations.go"))
	require.False(t, sqlPermitted("internal/repositoryx/a.go"))
	require.False(t, sqlPermitted("cmd/backend/serve.go"))
}

func isAdapter(rel string) bool {
	for _, dir := range adapterDirs {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}
	return false
}

// repositoryImports lists imports of the repository layer in one file, as
// "file:line: what".
func repositoryImports(name string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var out []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: import path: %w", name, err)
		}
		if path == repositoryImport || strings.HasPrefix(path, repositoryImport+"/") {
			out = append(out, fmt.Sprintf("%s:%d: import of %s", name, fset.Position(imp.Pos()).Line, path))
		}
	}
	return out, nil
}

func sqlPermitted(rel string) bool {
	if _, ok := allowList[rel]; ok {
		return true
	}
	for _, dir := range sqlAllowed {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}
	return false
}

// violations lists driver imports and SQL statements in string literals of
// one file, as "file:line: what".
func violations(name string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var out []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: import path: %w", name, err)
		}
		for _, prefix := range forbiddenImports {
			if strings.HasPrefix(path, prefix) {
				out = append(out, fmt.Sprintf("%s:%d: import of %s", name, fset.Position(imp.Pos()).Line, path))
			}
		}
	}
	imports := importPaths(f)
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if _, isImport := imports[lit]; isImport {
			return true
		}
		for _, re := range sqlStatements {
			if loc := re.FindStringIndex(lit.Value); loc != nil {
				line := fset.Position(lit.Pos()).Line + strings.Count(lit.Value[:loc[0]], "\n")
				out = append(out, fmt.Sprintf("%s:%d: SQL statement (%s)", name, line, lit.Value[loc[0]:loc[1]]))
				break
			}
		}
		return true
	})
	return out, nil
}

func importPaths(f *ast.File) map[*ast.BasicLit]struct{} {
	m := make(map[*ast.BasicLit]struct{}, len(f.Imports))
	for _, imp := range f.Imports {
		m[imp.Path] = struct{}{}
	}
	return m
}

// walkGoFiles calls fn for every non-test Go file of the module, with its
// path relative to the module root (slash-separated) and its source.
func walkGoFiles(root string, fn func(rel string, src []byte) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: paths come from walking the module tree
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(rel), src)
	})
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !errors.Is(err, fs.ErrNotExist) {
			require.NoError(t, err)
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found")
		dir = parent
	}
}
