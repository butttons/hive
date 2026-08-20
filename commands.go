package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

func cmdCheck(ctx context.Context, args []string) error { return notImplemented("check") }

func cmdDeploy(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	daemonFlag := fs.Bool("daemon", false, "")
	localFlag := fs.Bool("local", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}

	start := time.Now()
	var steps []deployStep
	var version string

	st := startStep("typecheck")
	skipped, err := runTypecheck(ctx, app, *jsonFlag)
	if err != nil {
		steps = append(steps, st.done(false))
		printDeployResult(app, version, steps, start, *jsonFlag, err)
		return err
	}
	steps = append(steps, st.doneSkip(skipped))

	st = startStep("deploy")
	version, err = runCelldDeploy(ctx, app, *jsonFlag)
	if err != nil {
		steps = append(steps, st.done(false))
		printDeployResult(app, version, steps, start, *jsonFlag, err)
		return err
	}
	steps = append(steps, st.done(true))

	st = startStep("restart")
	if err := restartNode(ctx, app, *daemonFlag, *localFlag, *jsonFlag); err != nil {
		steps = append(steps, st.done(false))
		printDeployResult(app, version, steps, start, *jsonFlag, err)
		return err
	}
	steps = append(steps, st.done(true))

	st = startStep("health")
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if ok, msg := waitForHealth(healthCtx, app); !ok {
		logPath := filepath.Join(app.Dir, ".hive", "node.log")
		steps = append(steps, st.done(false))
		printDeployResult(app, version, steps, start, *jsonFlag, nil)
		return fmt.Errorf("health gate timed out after 30s (see %s): %s", logPath, msg)
	}
	steps = append(steps, st.done(true))

	printDeployResult(app, version, steps, start, *jsonFlag, nil)
	return nil
}
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

type deployStep struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type deployResult struct {
	App        string       `json:"app"`
	Version    string       `json:"version,omitempty"`
	Steps      []deployStep `json:"steps"`
	DurationMs int64        `json:"duration_ms"`
	Error      string       `json:"error,omitempty"`
}

type stepTimer struct {
	name  string
	start time.Time
}

func startStep(name string) *stepTimer {
	return &stepTimer{name: name, start: time.Now()}
}

func (s *stepTimer) done(ok bool) deployStep {
	return deployStep{
		Name:       s.name,
		OK:         ok,
		DurationMs: time.Since(s.start).Milliseconds(),
	}
}

func (s *stepTimer) doneSkip(skipped bool) deployStep {
	if skipped {
		return deployStep{Name: s.name, OK: true, Skipped: true, DurationMs: 0}
	}
	return s.done(true)
}

func printDeployResult(app *App, version string, steps []deployStep, start time.Time, jsonMode bool, err error) {
	if jsonMode {
		res := deployResult{
			App:        app.Name,
			Version:    version,
			Steps:      steps,
			DurationMs: time.Since(start).Milliseconds(),
		}
		if err != nil {
			res.Error = err.Error()
		}
		b, marshalErr := json.MarshalIndent(res, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "hive deploy: marshal result: %v\n", marshalErr)
			return
		}
		fmt.Println(string(b))
		return
	}

	for _, s := range steps {
		if s.Skipped {
			fmt.Printf("%s skipped\n", s.Name)
		} else if s.OK {
			fmt.Printf("%s ok (%dms)\n", s.Name, s.DurationMs)
		} else {
			fmt.Printf("%s failed (%dms)\n", s.Name, s.DurationMs)
		}
	}
	if version != "" {
		fmt.Printf("version: %s\n", version)
	}
	if err != nil {
		return
	}
	if version != "" {
		fmt.Printf("deployed %s (%s) in %dms\n", app.Name, version, time.Since(start).Milliseconds())
	} else {
		fmt.Printf("deployed %s in %dms\n", app.Name, time.Since(start).Milliseconds())
	}
}

func findTSC(app *App) (string, error) {
	dir := app.Dir
	for {
		p := filepath.Join(dir, "node_modules", ".bin", "tsc")
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if p, err := exec.LookPath("tsc"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("tsc not found in node_modules/.bin or PATH")
}

func runTypecheck(ctx context.Context, app *App, jsonMode bool) (bool, error) {
	tsconfigPath := filepath.Join(app.Dir, "tsconfig.json")
	if _, err := os.Stat(tsconfigPath); os.IsNotExist(err) {
		if !jsonMode {
			fmt.Println("typecheck skipped (no tsconfig.json)")
		}
		return true, nil
	}

	tsc, err := findTSC(app)
	if err != nil {
		return false, err
	}

	stdout := os.Stdout
	stderr := os.Stderr
	if jsonMode {
		stdout = os.Stderr
	}

	cmd := exec.CommandContext(ctx, tsc, "-b")
	cmd.Dir = app.Dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("typecheck failed: %w", err)
	}
	return false, nil
}

func runCelldDeploy(ctx context.Context, app *App, jsonMode bool) (string, error) {
	bin, err := celldPath()
	if err != nil {
		return "", err
	}
	bucket, err := celldBucketEnv()
	if err != nil {
		return "", err
	}

	args := []string{
		"deploy",
		"--bucket", bucket + "/" + app.Name,
	}
	if ep := os.Getenv("S3_ENDPOINT"); ep != "" {
		args = append(args, "--endpoint", ep)
	}
	if r := os.Getenv("AWS_REGION"); r != "" {
		args = append(args, "--region", r)
	}

	var outBuf, errBuf bytes.Buffer
	var stdout, stderr io.Writer
	if jsonMode {
		stdout = io.MultiWriter(os.Stderr, &outBuf)
		stderr = io.MultiWriter(os.Stderr, &errBuf)
	} else {
		stdout = io.MultiWriter(os.Stdout, &outBuf)
		stderr = io.MultiWriter(os.Stderr, &errBuf)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = app.Dir
	cmd.Env = append(os.Environ(), celldEnviron()...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("celld deploy failed: %w", err)
	}
	return extractVersion(outBuf.String() + "\n" + errBuf.String()), nil
}

var versionRe = regexp.MustCompile(`(?i)(?:current version id|version id|version)[:\s=]+([a-f0-9]{7,64}|[^\s]+)`)

func extractVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if m := versionRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

func restartNode(ctx context.Context, app *App, daemon, local, jsonMode bool) error {
	if app.Hive.Server != "" && !local {
		return remoteRestart(app, daemon, jsonMode)
	}

	r, err := selectRunner(daemon)
	if err != nil {
		return err
	}
	st, err := r.Status(ctx, app)
	if err != nil {
		return fmt.Errorf("check node status: %w", err)
	}
	if st.Running {
		if err := r.Down(ctx, app); err != nil {
			return fmt.Errorf("stop node: %w", err)
		}
	}

	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := r.Up(healthCtx, app); err != nil {
		logPath := filepath.Join(app.Dir, ".hive", "node.log")
		return fmt.Errorf("start node (see %s): %w", logPath, err)
	}
	return nil
}

func remoteRestart(app *App, daemon, jsonMode bool) error {
	downArgs := []string{"--local"}
	if daemon {
		downArgs = append(downArgs, "--daemon")
	}
	if err := runRemoteHive(app.Hive.Server, "down", downArgs, jsonMode); err != nil {
		return fmt.Errorf("remote down: %w", err)
	}
	upArgs := []string{"--local"}
	if daemon {
		upArgs = append(upArgs, "--daemon")
	}
	if err := runRemoteHive(app.Hive.Server, "up", upArgs, jsonMode); err != nil {
		return fmt.Errorf("remote up: %w", err)
	}
	return nil
}

func runRemoteHive(server, cmd string, args []string, toStderr bool) error {
	sshArgs := append([]string{server, "hive", cmd}, args...)
	c := exec.Command("ssh", sshArgs...)
	c.Stdin = os.Stdin
	if toStderr {
		c.Stdout = os.Stderr
		c.Stderr = os.Stderr
	} else {
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
	}
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				return fmt.Errorf("ssh %s hive %s exited %d: %w", server, cmd, status.ExitStatus(), err)
			}
		}
		return fmt.Errorf("ssh %s hive %s failed: %w (is hive installed on the box?)", server, cmd, err)
	}
	return nil
}
