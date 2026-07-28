// Command nimbus is the zero-dependency entry point for the Nimbus wrapper.
//
// It is deliberately built with no third-party CLI framework: the binary must
// stay small, statically linked, and free of runtime dependencies so it can be
// the very first thing installed on a bare device (DESIGN.md §2a).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nkoteb/nimbus/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\naborted")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}
}
