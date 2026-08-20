package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// envFileKeys are the environment variables persisted for a celld node.
// They are written to ~/.config/hive/<app>.env, loaded by loadAppEnv for
// local commands and synced to servers over ssh. Secrets (AWS_*) never
// appear in generated config committed anywhere.
var envFileKeys = []string{
	"PATH",
	"HOME",
	"CELLD_BUCKET",
	"CELLD_ESBUILD",
	"S3_ENDPOINT",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"RUST_LOG",
}

func appEnvFilePath(app *App) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hive", app.Name+".env")
}

// readEnvFile parses a shell-sourcable env file into a key/value map.
// Empty lines, comments, and malformed lines are ignored. Values are
// unquoted using shell single-quote rules and CELLD_BUCKET is normalized
// to drop the s3:// prefix.
func readEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kv := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = unquoteShell(v)
		if k == "CELLD_BUCKET" {
			v = strings.TrimPrefix(v, "s3://")
		}
		kv[k] = v
	}
	return kv, nil
}

// loadAppEnv sources ~/.config/hive/<app>.env into the process environment
// for keys that are not already set. It lets a box keep credentials out of
// plist/unit files while still making them available to local commands.
func loadAppEnv(app *App) error {
	path := appEnvFilePath(app)
	kv, err := readEnvFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read env file %s: %w", path, err)
	}
	for k, v := range kv {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return nil
}

// buildAppEnvFileContent serializes the current environment's celld-relevant
// keys as a shell-sourcable file. Values are single-quoted for safety.
// CELLD_BUCKET is normalized to drop the s3:// prefix so the wrapper's sourced
// env matches what celld's --bucket argument expects.
func buildAppEnvFileContent() string {
	var b strings.Builder
	for _, k := range envFileKeys {
		if v := os.Getenv(k); v != "" {
			if k == "CELLD_BUCKET" {
				v = strings.TrimPrefix(v, "s3://")
			}
			fmt.Fprintf(&b, "%s=%s\n", k, shellSingleQuote(v))
		}
	}
	return b.String()
}

// syncAppEnvFile copies the local celld environment to the same path on the
// remote server. It is a no-op if CELD_BUCKET is not set locally, so an
// existing remote env file is not accidentally wiped.
func syncAppEnvFile(server string, app *App) error {
	if os.Getenv("CELLD_BUCKET") == "" {
		return nil
	}
	content := buildAppEnvFileContent()
	// $HOME expands on the remote shell; only the filename is quoted.
	remotePath := "$HOME/.config/hive/" + shellSingleQuote(app.Name+".env")
	script := fmt.Sprintf("umask 077; mkdir -p $HOME/.config/hive; cat > %s; chmod 600 %s",
		remotePath, remotePath)
	cmd := exec.Command("ssh", server, script)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sync env file to %s: %s: %w", server, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdEnv prints the effective celld environment for the current app:
// process env overlaid on ~/.config/hive/<app>.env. Output is shell-sourcable
// (eval "$(hive env)" or redirect into a .env). --tunnel also emits
// TUNNEL_TOKEN for the app's Cloudflare tunnel (needs cf auth + domain).
func cmdEnv(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{"name": true})
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	tunnelFlag := fs.Bool("tunnel", false, "also print TUNNEL_TOKEN (needs cf auth and a domain)")
	name := fs.String("name", "hive", "tunnel name (with --tunnel)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if err := loadAppEnv(app); err != nil {
		return err
	}

	out := make(map[string]string)
	for _, k := range envFileKeys {
		if k == "PATH" || k == "HOME" {
			continue
		}
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}

	if *tunnelFlag {
		if app.Hive.Domain == "" {
			return fmt.Errorf("no domain in the hive block of %s/package.json", app.Dir)
		}
		zone, err := findZone(ctx, app.Hive.Domain)
		if err != nil {
			return err
		}
		tunnel, err := findTunnel(ctx, zone.Account.ID, *name)
		if err != nil {
			return err
		}
		if tunnel == nil {
			return fmt.Errorf("no tunnel %q (run hive cf tunnel first)", *name)
		}
		tok, err := tunnelToken(ctx, zone.Account.ID, tunnel.ID)
		if err != nil {
			return err
		}
		out["TUNNEL_TOKEN"] = tok
	}

	if len(out) == 0 {
		return fmt.Errorf("no env vars found (export them, or run hive init)")
	}
	if *jsonFlag {
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal env: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, shellSingleQuote(out[k]))
	}
	return nil
}

func unquoteShell(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2 {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `'\''`, "'")
	}
	return s
}
