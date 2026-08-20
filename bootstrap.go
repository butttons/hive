package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// cmdBootstrap installs or upgrades hive and celld at ~/.local/bin on the
// app's server. Safe to re-run; the installers always fetch the latest
// release.
func cmdBootstrap(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if app.Hive.Server == "" {
		return fmt.Errorf("no server in the hive block of %s/package.json; bootstrap is for remote boxes", app.Dir)
	}

	script := "set -e; " +
		"curl -fsSL https://hive.butttons.dev/setup.sh | bash >/dev/null; " +
		"curl -fsSL https://celld.dev/install.sh | sh >/dev/null; " +
		"$HOME/.local/bin/celld --version 2>/dev/null | head -1"
	cmd := exec.CommandContext(ctx, "ssh", app.Hive.Server, script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootstrap %s: %s: %w", app.Hive.Server, strings.TrimSpace(string(out)), err)
	}
	version := strings.TrimSpace(string(out))
	if i := strings.LastIndex(version, "\n"); i >= 0 {
		version = version[i+1:]
	}

	if *jsonFlag {
		res := struct {
			Server string `json:"server"`
			Celld  string `json:"celld"`
		}{app.Hive.Server, version}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("bootstrapped %s (hive + %s)\n", app.Hive.Server, version)
	return nil
}
