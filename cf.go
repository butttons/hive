package main

import (
	"context"
	"fmt"
)

func cmdCF(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hive cf <login|tunnel>")
	}
	switch args[0] {
	case "login":
		return cmdCFLogin(ctx, args[1:])
	case "tunnel":
		return cmdCFTunnel(ctx, args[1:])
	default:
		return fmt.Errorf("unknown cf command: %s (available: login, tunnel)", args[0])
	}
}
