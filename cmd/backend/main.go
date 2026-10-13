// Command backend is the radio automation backend.
//
//	backend serve        HTTP API, WebSocket hub, background jobs
//	backend migrate up   apply database migrations and exit
//	backend openapi      print the OpenAPI specification (JSON) to stdout
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
)

const usage = `usage: backend <command>

commands:
  serve        run the HTTP API, WebSocket hub, and background jobs
  migrate up   apply database migrations and exit
  openapi      print the OpenAPI specification (JSON) to stdout
`

// version is the release version, set at build time with
// -ldflags "-X main.version=1.2.3".
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
			os.Exit(2)
		}
		os.Exit(1)
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
		if len(rest) != 1 || rest[0] != "up" {
			return fmt.Errorf("%w: only `migrate up` exists (migrations are forward-only)", errUsage)
		}
		return migrateUp(ctx, stdout)
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
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}
