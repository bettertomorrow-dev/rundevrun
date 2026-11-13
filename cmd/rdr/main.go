// rdr manages development processes described by dev.toml.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bettertomorrow-dev/rundevrun/internal/cli"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/publication"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "__rdr_publication" {
		if err := publication.Supervise(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) >= 3 && os.Args[1] == "__rdr_publication_bootstrap" {
		if err := publication.Bootstrap(os.Args[2:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "__rdr_proxy" {
		if err := proxy.Serve(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "__rdr_supervisor" {
		if err := runner.Supervise(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "rdr supervisor:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "__rdr_bootstrap" {
		if err := runner.Bootstrap(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
