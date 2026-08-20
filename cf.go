package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

func cmdCF(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("cf", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if len(fs.Args()) == 0 {
		return fmt.Errorf("usage: hive cf <login|tunnel>")
	}
	sub := fs.Args()[0]
	switch sub {
	case "login":
		return cmdCFLogin(ctx, fs.Args()[1:])
	case "tunnel":
		return cmdCFTunnel(ctx, fs.Args()[1:])
	default:
		return fmt.Errorf("unknown cf command: %s (available: login, tunnel)", sub)
	}
}
