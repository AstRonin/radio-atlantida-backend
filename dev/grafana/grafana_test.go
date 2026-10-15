//go:build integration

// Package grafana_test checks the monitoring files of the dev stack against
// a real PostgreSQL: the role script role.sql and the queries of the
// provisioned dashboards (docs/phase-14-plan.md §5). Every panel must use a
// provisioned data source; SQL panels run their query, Loki panels
// (logs.json, docs/phase-15-plan.md) are only checked for it, since their
// pipeline is tested in dev/alloy. The files are copied to
// radio-atlantida-deploy, so this test guards both.
package grafana_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
)

const (
	role = "grafana_ro"
	// passwordOption is the session setting role.sql reads the password from.
	passwordOption = "radio.grafana_password" //nolint:gosec // G101: the name of a setting, not a credential
)

func TestRoleScript(t *testing.T) {
	t.Parallel()
	dsn := dbtest.StartPostgres(t)
	script := readFile(t, "role.sql")

	admin := connect(t, dsn, nil)
	exec(t, admin, `CREATE TABLE tracks (id int NOT NULL); INSERT INTO tracks VALUES (1)`)

	err := runScript(t, dsn, script, "")
	require.ErrorContains(t, err, "radio.grafana_password is not set")

	// Twice: the script is idempotent and the second run changes the password.
	require.NoError(t, runScript(t, dsn, script, "first-pass"))
	require.NoError(t, runScript(t, dsn, script, "second-pass"))

	_, err = pgx.Connect(t.Context(), roleURL(t, dsn, "first-pass"))
	require.Error(t, err, "the old password must no longer work")

	ro := connect(t, roleURL(t, dsn, "second-pass"), nil)

	t.Run("role attributes", func(t *testing.T) {
		var connLimit int
		var super, monitor bool
		err := admin.QueryRow(t.Context(),
			`SELECT rolconnlimit, rolsuper, pg_has_role(oid, 'pg_monitor', 'USAGE') FROM pg_roles WHERE rolname = $1`,
			role).Scan(&connLimit, &super, &monitor)
		require.NoError(t, err)
		require.Equal(t, 3, connLimit)
		require.False(t, super)
		require.True(t, monitor)
	})

	t.Run("settings", func(t *testing.T) {
		tests := []struct{ name, want string }{
			{"statement_timeout", "5s"},
			{"idle_in_transaction_session_timeout", "10s"},
			{"default_transaction_read_only", "on"},
		}
		for _, tt := range tests {
			var got string
			require.NoError(t, ro.QueryRow(t.Context(), "SHOW "+tt.name).Scan(&got))
			require.Equal(t, tt.want, got, tt.name)
		}
	})

	t.Run("no access to application data", func(t *testing.T) {
		tests := []struct{ name, sql, code string }{
			{"read", `SELECT id FROM tracks`, "42501"},          // insufficient_privilege
			{"write", `INSERT INTO tracks VALUES (2)`, "25006"}, // read_only_sql_transaction
			{"create", `CREATE TABLE extra (id int)`, "25006"},
		}
		for _, tt := range tests {
			_, err := ro.Exec(t.Context(), tt.sql)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, tt.name)
			require.Equal(t, tt.code, pgErr.Code, tt.name)
		}
	})

	t.Run("sees other sessions' queries", func(t *testing.T) {
		var adminPID int
		require.NoError(t, admin.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&adminPID))

		var query string
		err := ro.QueryRow(t.Context(), `SELECT query FROM pg_stat_activity WHERE pid = $1`, adminPID).Scan(&query)
		require.NoError(t, err)
		require.Equal(t, `SELECT pg_backend_pid()`, query)
	})
}

func TestDashboardQueries(t *testing.T) {
	t.Parallel()
	dsn := dbtest.StartPostgres(t)
	// A table, so the table panels return rows.
	exec(t, connect(t, dsn, nil), `CREATE TABLE tracks (id int NOT NULL); INSERT INTO tracks VALUES (1); ANALYZE tracks`)
	require.NoError(t, runScript(t, dsn, readFile(t, "role.sql"), "dashboard-pass"))
	ro := connect(t, roleURL(t, dsn, "dashboard-pass"), nil)

	sources := provisionedSources(t)
	require.Equal(t, map[string]string{"radio-db": postgresType, "radio-logs": lokiType}, sources)

	files, err := filepath.Glob(filepath.Join("dashboards", "*.json"))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"database.json", "logs.json"}, baseNames(files))

	for _, file := range files {
		var dash dashboard
		require.NoError(t, json.Unmarshal(readFile(t, file), &dash), file)
		require.NotEmpty(t, dash.Panels, file)

		for _, v := range dash.Templating.List {
			if v.Datasource.UID != "" { // a text box has none
				require.Contains(t, sources, v.Datasource.UID, "%s: variable %s", file, v.Name)
				require.Equal(t, sources[v.Datasource.UID], v.Datasource.Type, "%s: variable %s", file, v.Name)
			}
		}

		for _, p := range dash.allPanels() {
			t.Run(filepath.Base(file)+"/"+p.Title, func(t *testing.T) {
				require.NotEmpty(t, p.Targets)
				require.Contains(t, sources, p.Datasource.UID, "panel data source")
				require.Equal(t, sources[p.Datasource.UID], p.Datasource.Type, "panel data source")
				for _, target := range p.Targets {
					require.Equal(t, p.Datasource, target.Datasource)
					if target.Datasource.Type == lokiType {
						require.NotEmpty(t, target.Expr)
						continue
					}
					require.NotEmpty(t, target.RawSQL)

					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					rows, err := ro.Query(ctx, target.RawSQL)
					require.NoError(t, err)
					for rows.Next() {
					}
					require.NoError(t, rows.Err(), target.RawSQL)
				}
			})
		}
	}
}

// Data source types of the provisioning files.
const (
	postgresType = "grafana-postgresql-datasource"
	lokiType     = "loki"
)

// provisionedSources maps the uid of every provisioned data source to its
// type.
func provisionedSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("provisioning", "datasources", "*.yaml"))
	require.NoError(t, err)
	out := map[string]string{}
	for _, file := range files {
		var provisioning struct {
			Datasources []struct {
				UID  string `yaml:"uid"`
				Type string `yaml:"type"`
			} `yaml:"datasources"`
		}
		require.NoError(t, yaml.Unmarshal(readFile(t, file), &provisioning), file)
		for _, ds := range provisioning.Datasources {
			require.NotContains(t, out, ds.UID, "uid used twice")
			out[ds.UID] = ds.Type
		}
	}
	return out
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// dashboard holds the parts of a Grafana dashboard JSON the test reads.
type dashboard struct {
	Panels     []panel `json:"panels"`
	Templating struct {
		List []struct {
			Name       string     `json:"name"`
			Datasource datasource `json:"datasource"`
		} `json:"list"`
	} `json:"templating"`
}

type panel struct {
	Title      string     `json:"title"`
	Type       string     `json:"type"`
	Datasource datasource `json:"datasource"`
	Targets    []struct {
		Datasource datasource `json:"datasource"`
		RawSQL     string     `json:"rawSql"`
		Expr       string     `json:"expr"` // LogQL
	} `json:"targets"`
	Panels []panel `json:"panels"` // inside a collapsed row
}

type datasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// allPanels returns the panels that query data, rows flattened.
func (d dashboard) allPanels() []panel {
	var out []panel
	var walk func([]panel)
	walk = func(ps []panel) {
		for _, p := range ps {
			if p.Type == "row" {
				walk(p.Panels)
				continue
			}
			out = append(out, p)
		}
	}
	walk(d.Panels)
	return out
}

// runScript runs role.sql like the grafana-role service: one session as the
// superuser, the password in the session setting (empty: not set).
func runScript(t *testing.T, dsn string, script []byte, password string) error {
	t.Helper()
	params := map[string]string{}
	if password != "" {
		params[passwordOption] = password
	}
	conn := connect(t, dsn, params)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// The simple protocol runs the statements of the file in one call.
	_, err := conn.PgConn().Exec(ctx, string(script)).ReadAll()
	return err
}

func connect(t *testing.T, dsn string, params map[string]string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	for k, v := range params {
		cfg.RuntimeParams[k] = v
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := conn.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("close connection: %v", err)
		}
	})
	return conn
}

// roleURL is dsn with the user and password of the Grafana role.
func roleURL(t *testing.T, dsn, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	return u.String()
}

func exec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	_, err := conn.PgConn().Exec(t.Context(), sql).ReadAll()
	require.NoError(t, err)
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name) //nolint:gosec // G304: fixed file names of this directory
	require.NoError(t, err)
	return b
}
