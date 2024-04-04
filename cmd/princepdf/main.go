package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// build data provided by goreleaser and mage setup
var (
	name    = "princepdf"
	version = "dev"
	date    = ""
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return root().cmd().ExecuteContext(ctx)
}
