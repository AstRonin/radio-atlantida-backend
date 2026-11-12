// Command backend is the radio automation backend.
//
//	backend serve                  HTTP API, WebSocket hub, background jobs
//	backend migrate up             apply database migrations and exit
//	backend migrate check          print the schema compatibility (JSON)
//	backend migrate target-compat  print the min_app_version this release's migrations set
//	backend openapi                print the OpenAPI specification (JSON) to stdout
//	backend version                print the release version
//
// Exit codes: 0 success, 1 error (for `migrate up`: nothing was changed),
// 2 usage error, 3 the database schema needs a newer release (`migrate
// check`, `serve`), 4 a migration failed and was rolled back (the schema is
// clean at the previous version), 5 the database is dirty (the deploy tool
// restores the pre-update dump).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

const usage = `usage: backend <command>

commands:
  serve                  run the HTTP API, WebSocket hub, and background jobs
  migrate up             apply database migrations and exit
                         (exit 4: a migration failed and was rolled back;
                         exit 5: the database is dirty)
  migrate check          print the schema version, the dirty flag, min_app_version,
                         and whether this binary may run against the schema, as JSON
                         (exit 3: it may not)
  migrate target-compat  print the min_app_version that this release's migrations set
  openapi                print the OpenAPI specification (JSON) to stdout
  version                print the release version
`

// version is the release version, set at build time with
// -ldflags "-X main.version=1.2.3" (the Dockerfile's VERSION, make build).
var version = "dev"

// errUsage marks a command-line error; the usage text is printed with it.
var errUsage = errors.New("invalid arguments")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "backend:", err)
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
		}
		os.Exit(exitCode(err))
	}
}

// exitCode maps an error of run to the process exit code. The deploy tool
// reads 3, 4, and 5 (deploy plan §7.3 step 7).
func exitCode(err error) int {
	switch {
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, errIncompatible):
		return 3
	case errors.Is(err, migrations.ErrRolledBack):
		return 4
	case errors.Is(err, migrations.ErrDirty):
		return 5
	default:
		return 1
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing command", errUsage)
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		if len(rest) != 0 {
			return fmt.Errorf("%w: serve takes no arguments", errUsage)
		}
		return serve(ctx)
	case "migrate":
		sub := ""
		if len(rest) == 1 {
			sub = rest[0]
		}
		switch sub {
		case "up":
			return migrateUp(ctx, stdout)
		case "check":
			return migrateCheck(ctx, stdout)
		case "target-compat":
			_, err := fmt.Fprintln(stdout, migrations.TargetMinAppVersion)
			return err
		default:
			return fmt.Errorf("%w: migrate takes one of up, check, target-compat (migrations are forward-only, there is no down)", errUsage)
		}
	case "openapi":
		if len(rest) != 0 {
			return fmt.Errorf("%w: openapi takes no arguments", errUsage)
		}
		doc, err := httpapi.OpenAPI(version)
		if err != nil {
			return err
		}
		_, err = stdout.Write(doc)
		return err
	case "version":
		if len(rest) != 0 {
			return fmt.Errorf("%w: version takes no arguments", errUsage)
		}
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}
