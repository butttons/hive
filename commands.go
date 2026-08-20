package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

func cmdCheck(ctx context.Context, args []string) error  { return notImplemented("check") }
func cmdDeploy(ctx context.Context, args []string) error { return notImplemented("deploy") }
func cmdInit(ctx context.Context, args []string) error   { return notImplemented("init") }
func cmdLogin(ctx context.Context, args []string) error  { return notImplemented("login") }
func cmdTunnel(ctx context.Context, args []string) error { return notImplemented("tunnel") }
func cmdUI(ctx context.Context, args []string) error     { return notImplemented("ui") }

func loadCwdApp() (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	return LoadApp(cwd)
}

func remoteHive(server, cmd string, args []string) error {
	sshArgs := append([]string{server, "hive", cmd}, args...)
	c := exec.Command("ssh", sshArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
		}
		return fmt.Errorf("ssh %s hive %s failed: %w (is hive installed on the box?)", server, cmd, err)
	}
	return nil
}

func cmdUp(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	daemonFlag := fs.Bool("daemon", false, "")
	localFlag := fs.Bool("local", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if app.Hive.Server != "" && !*localFlag {
		return remoteHive(app.Hive.Server, "up", append(fs.Args(), "--local"))
	}

	r, err := selectRunner(*daemonFlag)
	if err != nil {
		return err
	}
	return r.Up(ctx, app)
}

func cmdDown(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	localFlag := fs.Bool("local", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if app.Hive.Server != "" && !*localFlag {
		return remoteHive(app.Hive.Server, "down", append(fs.Args(), "--local"))
	}

	// Stop whichever backend is actually managing the node. Prefer daemon
	// backends: a launchd/systemd job will restart a process we SIGTERM.
	for _, r := range []runner{launchdRunner{}, systemdRunner{}} {
		if r.Name() == "launchd" {
			loaded, _ := launchdList(app)
			if loaded {
				return r.Down(ctx, app)
			}
		}
		if r.Name() == "systemd" {
			active, _ := systemdIsActive(app)
			if active {
				return r.Down(ctx, app)
			}
		}
	}

	st, err := processStatus(ctx, app)
	if err != nil {
		return err
	}
	if st.Running {
		return processRunner{}.Down(ctx, app)
	}
	fmt.Printf("node for %s is already down\n", app.Name)
	return nil
}

type statusResult struct {
	App    App        `json:"app"`
	Server string     `json:"server,omitempty"`
	Node   nodeStatus `json:"node"`
}

func cmdStatus(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	localFlag := fs.Bool("local", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if app.Hive.Server != "" && !*localFlag {
		return remoteHive(app.Hive.Server, "status", append(fs.Args(), "--local"))
	}

	var st nodeStatus
	for _, r := range []runner{processRunner{}, launchdRunner{}, systemdRunner{}} {
		st, err = r.Status(ctx, app)
		if err == nil && (st.Running || st.PID != 0) {
			break
		}
	}
	if err != nil {
		return err
	}
	st.Version = fetchLiveVersion(app)

	res := statusResult{App: *app, Server: app.Hive.Server, Node: st}
	if *jsonFlag {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal status: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}

	fmt.Printf("App:      %s\n", app.Name)
	fmt.Printf("Dir:      %s\n", app.Dir)
	fmt.Printf("Port:     %d\n", app.Hive.Port)
	if app.Hive.Domain != "" {
		fmt.Printf("Domain:   %s\n", app.Hive.Domain)
	}
	if app.Hive.Server != "" {
		fmt.Printf("Server:   %s\n", app.Hive.Server)
	}
	fmt.Printf("Node:     %s\n", map[bool]string{true: "running", false: "down"}[st.Running])
	if st.PID != 0 {
		fmt.Printf("PID:      %d\n", st.PID)
	}
	fmt.Printf("Backend:  %s\n", st.Backend)
	fmt.Printf("Health:   %s\n", map[bool]string{true: "ok", false: "unhealthy"}[st.Healthy])
	if st.HealthError != "" {
		fmt.Printf("Health detail: %s\n", st.HealthError)
	}
	if st.Version != "" {
		fmt.Printf("Version:  %s\n", st.Version)
	}
	return nil
}

func fetchLiveVersion(app *App) string {
	bucket := os.Getenv("CELLD_BUCKET")
	if bucket == "" {
		return "unknown"
	}
	// Try to read the latest deployment manifest from the local object store.
	// This is best-effort: credentials may be missing or the store unreachable.
	return "unknown"
}
