package main

import (
	"context"
	"os"

	"github.com/wrouesnel/ctxstdio"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/entrypoints/frpva"
)

func main() {
	// The real entry point is in the entrypoint package, which allows for efficient test integration.
	// Do not add more code to this file (it should also be excluded from coverage tracking).
	exitCode := frpva.Entrypoint(ctxstdio.Set(context.Background(), os.Stdout, os.Stderr, os.Stdin), os.Args[1:])
	os.Exit(exitCode)
}
