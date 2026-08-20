package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var exeVMNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func cmdExe(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("exe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if len(fs.Args()) == 0 {
		return fmt.Errorf("usage: hive exe <new|share|domain>")
	}
	sub := fs.Args()[0]
	switch sub {
	case "new":
		return cmdExeNew(ctx, fs.Args()[1:])
	case "share":
		return cmdExeShare(ctx, fs.Args()[1:])
	case "domain":
		return cmdExeDomain(ctx, fs.Args()[1:])
	default:
		return fmt.Errorf("unknown exe command: %s (available: new, share, domain)", sub)
	}
}

// exeVMName extracts the exe.dev VM name from the app's server field,
// accepting "<name>.exe.xyz" or "user@<name>.exe.xyz".
func exeVMName(app *App) (string, error) {
	s := app.Hive.Server
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	name, ok := strings.CutSuffix(s, ".exe.xyz")
	if !ok || !exeVMNameRe.MatchString(name) {
		return "", fmt.Errorf("server %q is not an exe.dev VM (want <name>.exe.xyz)", app.Hive.Server)
	}
	return name, nil
}

// exeCLI runs one exe.dev control command over ssh. Arguments are joined
// by ssh with plain spaces, so callers must only pass simple tokens (VM
// names are validated, ports are ints, domains are hostnames).
func exeCLI(ctx context.Context, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "ssh", append([]string{"exe.dev"}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh exe.dev %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// waitForDNS polls the resolver until host resolves or the timeout hits.
// Fresh exe.dev VMs and DNS records take tens of seconds to propagate.
func waitForDNS(ctx context.Context, host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := net.LookupHost(host); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to resolve", host)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func cmdExeNew(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("exe new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if len(fs.Args()) != 1 {
		return fmt.Errorf("usage: hive exe new <name>")
	}
	name := fs.Args()[0]
	if !exeVMNameRe.MatchString(name) {
		return fmt.Errorf("invalid VM name %q (lowercase letters, digits, dashes)", name)
	}

	if _, err := exeCLI(ctx, "new", "--name", name); err != nil {
		return err
	}
	host := name + ".exe.xyz"
	if err := waitForDNS(ctx, host, 2*time.Minute); err != nil {
		return err
	}

	if *jsonFlag {
		res := struct {
			Name     string `json:"name"`
			Hostname string `json:"hostname"`
		}{name, host}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("created vm %q\n", name)
	fmt.Printf("ssh:   ssh %s\n", host)
	fmt.Printf("https: https://%s\n", host)
	return nil
}

func cmdExeShare(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("exe share", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	private := fs.Bool("private", false, "keep the proxy behind exe.dev login")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	vm, err := exeVMName(app)
	if err != nil {
		return err
	}

	if _, err := exeCLI(ctx, "share", "port", vm, strconv.Itoa(app.Hive.Port)); err != nil {
		return err
	}
	visibility := "set-public"
	if *private {
		visibility = "set-private"
	}
	if _, err := exeCLI(ctx, "share", visibility, vm); err != nil {
		return err
	}

	if *jsonFlag {
		res := struct {
			VM         string `json:"vm"`
			Port       int    `json:"port"`
			Visibility string `json:"visibility"`
			URL        string `json:"url"`
		}{vm, app.Hive.Port, strings.TrimPrefix(visibility, "set-"), "https://" + vm + ".exe.xyz"}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("https://%s.exe.xyz -> port %d (%s)\n", vm, app.Hive.Port, strings.TrimPrefix(visibility, "set-"))
	return nil
}

func cmdExeDomain(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("exe domain", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	vm, err := exeVMName(app)
	if err != nil {
		return err
	}
	if app.Hive.Domain == "" {
		return fmt.Errorf("no domain in the hive block of %s/package.json", app.Dir)
	}
	target := vm + ".exe.xyz"

	if _, err := cfAccessToken(ctx); err != nil {
		return fmt.Errorf("no cloudflare credentials; create this record at your DNS provider, then re-run:\n  %s CNAME %s (DNS only, not proxied)", app.Hive.Domain, target)
	}
	zone, err := findZone(ctx, app.Hive.Domain)
	if err != nil {
		return err
	}
	changed, err := upsertCNAME(ctx, zone.ID, app.Hive.Domain, target, false)
	if err != nil {
		return err
	}
	if err := waitForDNS(ctx, app.Hive.Domain, 2*time.Minute); err != nil {
		return err
	}
	// exe.dev verifies DNS from its own resolver, which lags ours, and
	// `domain add` can report failure only on stdout. Retry until the
	// registration is visible in `domain ls`.
	var lastErr error
	registered := false
	for range 10 {
		if _, err := exeCLI(ctx, "domain", "add", vm, app.Hive.Domain); err != nil {
			lastErr = err
		}
		if out, err := exeCLI(ctx, "domain", "ls", vm); err == nil && strings.Contains(out, app.Hive.Domain) {
			registered = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if !registered {
		if lastErr != nil {
			return fmt.Errorf("register %s with exe.dev: %w", app.Hive.Domain, lastErr)
		}
		return fmt.Errorf("exe.dev did not register %s (their resolver cannot see the CNAME yet — re-run in a minute)", app.Hive.Domain)
	}

	if *jsonFlag {
		res := struct {
			VM         string `json:"vm"`
			Domain     string `json:"domain"`
			DNSChanged bool   `json:"dns_changed"`
			URL        string `json:"url"`
		}{vm, app.Hive.Domain, changed, "https://" + app.Hive.Domain}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	if changed {
		fmt.Printf("dns: %s -> %s (dns only)\n", app.Hive.Domain, target)
	} else {
		fmt.Printf("dns: %s already points at %s\n", app.Hive.Domain, target)
	}
	fmt.Printf("live: https://%s\n", app.Hive.Domain)
	return nil
}
