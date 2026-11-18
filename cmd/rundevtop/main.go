// Implements rundevtop main.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bettertomorrow-dev/rundevrun/internal/cli"
	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"github.com/bettertomorrow-dev/rundevrun/internal/tui"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "__rdr_proxy" {
		if err := proxy.Serve(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "__rdr_bootstrap" {
		if err := runner.Bootstrap(); err != nil {
			fmt.Fprintln(os.Stderr, "rundevtop bootstrap:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 {
		if os.Args[1] == "--version" || os.Args[1] == "version" {
			fmt.Println("rundevtop", cli.Version)
			return
		}
		if os.Args[1] == "--help" || os.Args[1] == "-h" {
			fmt.Println(tui.Help())
			return
		}
		fmt.Fprintln(os.Stderr, "rundevtop: unexpected argument:", os.Args[1])
		os.Exit(2)
	}
	root, err := monitor.RuntimeRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rundevtop:", err)
		os.Exit(1)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rundevtop:", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = tui.Run(ctx, &state.Store{Root: root}, cwd, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "rundevtop:", err)
		os.Exit(1)
	}
}
