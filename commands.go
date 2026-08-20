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
	"slices"
	"strings"
	"syscall"
	"time"
)

func cmdCheck(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	start := time.Now()
	app, err := loadCwdApp()
	if err != nil {
		return err
	}

	steps, ok := runChecks(ctx, app, *jsonFlag)
	printCheckResult(app, steps, start, *jsonFlag)
	if !ok {
		return fmt.Errorf("check failed")
	}
	return nil
}

// celldLegalKeys is the allowlist from celld's wrangler.jsonc parser.
// Derived from crates/celld/deploy.rs SUPPORTED_KEYS at celld v0.2.1.
var celldLegalKeys = map[string]bool{
	"$schema":             true,
	"name":                true,
	"main":                true,
	"compatibility_date":  true,
	"compatibility_flags": true,
	"durable_objects":     true,
	"migrations":          true,
	"assets":              true,
	"services":            true,
	"vars":                true,
	"no_bundle":           true,
}

// hiveOnlyKeys are wrangler.jsonc keys that celld rejects and that belong in
// package.json's "hive" block instead.
var hiveOnlyKeys = map[string]bool{
	"routes": true,
	"port":   true,
	"domain": true,
}

type checkStep struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type checkResult struct {
	App        string      `json:"app"`
	OK         bool        `json:"ok"`
	Checks     []checkStep `json:"checks"`
	DurationMs int64       `json:"duration_ms"`
}

func runChecks(ctx context.Context, app *App, jsonMode bool) ([]checkStep, bool) {
	var steps []checkStep
	overall := true

	st := startStep("config")
	wranglerPath := filepath.Join(app.Dir, "wrangler.jsonc")
	config, err := readWranglerObject(wranglerPath)
	if err != nil {
		steps = append(steps, checkStep{Name: st.name, OK: false, Error: err.Error(), DurationMs: time.Since(st.start).Milliseconds()})
		return steps, false
	}
	name, _ := config["name"].(string)
	main, _ := config["main"].(string)
	if name == "" {
		steps = append(steps, checkStep{Name: st.name, OK: false, Error: "wrangler.jsonc: missing \"name\"", DurationMs: time.Since(st.start).Milliseconds()})
		overall = false
	} else if main == "" {
		steps = append(steps, checkStep{Name: st.name, OK: false, Error: "wrangler.jsonc: missing \"main\"", DurationMs: time.Since(st.start).Milliseconds()})
		overall = false
	} else {
		steps = append(steps, checkStep{Name: st.name, OK: true, Detail: fmt.Sprintf("name=%s main=%s", name, main), DurationMs: time.Since(st.start).Milliseconds()})
	}

	st = startStep("keys")
	var illegal []string
	var shouldBeHive []string
	for k := range config {
		if !celldLegalKeys[k] {
			illegal = append(illegal, k)
			if hiveOnlyKeys[k] {
				shouldBeHive = append(shouldBeHive, k)
			}
		}
	}
	slices.Sort(illegal)
	slices.Sort(shouldBeHive)
	if len(illegal) > 0 {
		msg := fmt.Sprintf("celld does not support these config keys: %s", strings.Join(illegal, ", "))
		if len(shouldBeHive) > 0 {
			msg += fmt.Sprintf(". Move %s to package.json's \"hive\" block", strings.Join(shouldBeHive, ", "))
		}
		steps = append(steps, checkStep{Name: st.name, OK: false, Error: msg, DurationMs: time.Since(st.start).Milliseconds()})
		overall = false
	} else {
		steps = append(steps, checkStep{Name: st.name, OK: true, DurationMs: time.Since(st.start).Milliseconds()})
	}

	st = startStep("files")
	mainPath := filepath.Join(app.Dir, main)
	if _, err := os.Stat(mainPath); err != nil {
		steps = append(steps, checkStep{Name: st.name, OK: false, Error: fmt.Sprintf("main entry not found: %s", mainPath), DurationMs: time.Since(st.start).Milliseconds()})
		overall = false
	} else {
		steps = append(steps, checkStep{Name: st.name, OK: true, DurationMs: time.Since(st.start).Milliseconds()})
	}

	st = startStep("binary")
	if _, err := celldPath(); err != nil {
		steps = append(steps, checkStep{Name: st.name, OK: false, Error: err.Error(), DurationMs: time.Since(st.start).Milliseconds()})
		overall = false
	} else {
		steps = append(steps, checkStep{Name: st.name, OK: true, DurationMs: time.Since(st.start).Milliseconds()})
	}

	st = startStep("typecheck")
	tsconfigPath := filepath.Join(app.Dir, "tsconfig.json")
	if _, err := os.Stat(tsconfigPath); os.IsNotExist(err) {
		steps = append(steps, checkStep{Name: st.name, OK: true, Skipped: true, Detail: "no tsconfig.json", DurationMs: time.Since(st.start).Milliseconds()})
	} else {
		if _, err := findTSC(app); err != nil {
			steps = append(steps, checkStep{Name: st.name, OK: true, Skipped: true, Detail: "tsc not found (run npm install)", DurationMs: time.Since(st.start).Milliseconds()})
		} else if err := runCheckTypecheck(ctx, app, jsonMode); err != nil {
			steps = append(steps, checkStep{Name: st.name, OK: false, Error: err.Error(), DurationMs: time.Since(st.start).Milliseconds()})
			overall = false
		} else {
			steps = append(steps, checkStep{Name: st.name, OK: true, DurationMs: time.Since(st.start).Milliseconds()})
		}
	}

	st = startStep("bundle")
	if os.Getenv("CELLD_BUCKET") == "" {
		steps = append(steps, checkStep{Name: st.name, OK: true, Skipped: true, Detail: "CELLD_BUCKET not set", DurationMs: time.Since(st.start).Milliseconds()})
	} else {
		if err := runCelldDryRun(ctx, app, jsonMode); err != nil {
			steps = append(steps, checkStep{Name: st.name, OK: false, Error: err.Error(), DurationMs: time.Since(st.start).Milliseconds()})
			overall = false
		} else {
			steps = append(steps, checkStep{Name: st.name, OK: true, DurationMs: time.Since(st.start).Milliseconds()})
		}
	}

	return steps, overall
}

func readWranglerObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var v map[string]any
	if err := json.Unmarshal(stripJSONC(b), &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return v, nil
}

func runCheckTypecheck(ctx context.Context, app *App, jsonMode bool) error {
	_, err := runTypecheck(ctx, app, jsonMode)
	return err
}

func runCelldDryRun(ctx context.Context, app *App, jsonMode bool) error {
	bin, err := celldPath()
	if err != nil {
		return err
	}
	bucket, err := celldBucketEnv()
	if err != nil {
		return err
	}

	args := []string{
		"deploy",
		"--dry-run",
		"--bucket", bucket + "/" + app.Name,
	}
	if ep := os.Getenv("S3_ENDPOINT"); ep != "" {
		args = append(args, "--endpoint", ep)
	}
	if r := os.Getenv("AWS_REGION"); r != "" {
		args = append(args, "--region", r)
	}

	var stdout, stderr io.Writer
	if jsonMode {
		stdout = os.Stderr
		stderr = os.Stderr
	} else {
		stdout = os.Stdout
		stderr = os.Stderr
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = app.Dir
	cmd.Env = append(os.Environ(), celldEnviron()...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("celld deploy --dry-run failed: %w", err)
	}
	return nil
}

func printCheckResult(app *App, steps []checkStep, start time.Time, jsonMode bool) {
	ok := true
	for _, s := range steps {
		if !s.OK {
			ok = false
			break
		}
	}
	if jsonMode {
		res := checkResult{
			App:        app.Name,
			OK:         ok,
			Checks:     steps,
			DurationMs: time.Since(start).Milliseconds(),
		}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hive check: marshal result: %v\n", err)
			return
		}
		fmt.Println(string(b))
		return
	}

	for _, s := range steps {
		if s.Skipped {
			fmt.Printf("%s skipped (%s)\n", s.Name, s.Detail)
		} else if s.OK {
			fmt.Printf("%s ok (%dms)\n", s.Name, s.DurationMs)
		} else {
			fmt.Printf("%s failed (%dms): %s\n", s.Name, s.DurationMs, s.Error)
		}
	}
	if ok {
		fmt.Printf("%s can deploy to celld (%dms)\n", app.Name, time.Since(start).Milliseconds())
	} else {
		fmt.Printf("%s cannot deploy to celld (%dms)\n", app.Name, time.Since(start).Milliseconds())
	}
}

func cmdDeploy(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	dockerFlag := fs.Bool("docker", false, "")
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
	if err := restartNode(ctx, app, *dockerFlag, *localFlag, *jsonFlag); err != nil {
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
func cmdUI(ctx context.Context, args []string) error     { return notImplemented("ui") }

func loadCwdApp() (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	app, err := LoadApp(cwd)
	if err != nil {
		return nil, err
	}
	if err := loadAppEnv(app); err != nil {
		return nil, err
	}
	return app, nil
}

func remoteHive(server, dir, cmd string, args []string) error {
	remoteCmd := fmt.Sprintf("cd %s && ~/.local/bin/hive %s", shellSingleQuote(dir), shellJoin(append([]string{cmd}, args...)))
	c := exec.Command("ssh", server, remoteCmd)
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
		return fmt.Errorf("ssh %s ~/.local/bin/hive %s failed: %w (is hive installed on the box?)", server, cmd, err)
	}
	return nil
}

func shellJoin(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellSingleQuote(a))
	}
	return b.String()
}

func cmdUp(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dockerFlag := fs.Bool("docker", false, "")
	localFlag := fs.Bool("local", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if app.Hive.Server != "" && !*localFlag {
		if err := syncAppEnvFile(app.Hive.Server, app); err != nil {
			return err
		}
		remoteArgs := append([]string{}, fs.Args()...)
		if *dockerFlag {
			remoteArgs = append(remoteArgs, "--docker")
		}
		remoteArgs = append(remoteArgs, "--local")
		return remoteHive(app.Hive.Server, app.Dir, "up", remoteArgs)
	}

	return selectRunner(app, *dockerFlag).Up(ctx, app)
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
		return remoteHive(app.Hive.Server, app.Dir, "down", append(fs.Args(), "--local"))
	}

	// Stop whichever backend is actually managing the node: a docker
	// container first, then a plain process holding the port.
	_, _, exists, err := dockerInspect(ctx, app)
	if err != nil {
		return err
	}
	if exists {
		return dockerRunner{}.Down(ctx, app)
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
		return remoteHive(app.Hive.Server, app.Dir, "status", append(fs.Args(), "--local"))
	}

	var st nodeStatus
	for _, r := range []runner{dockerRunner{}, processRunner{}} {
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

func restartNode(ctx context.Context, app *App, dockerFlag, local, jsonMode bool) error {
	if app.Hive.Server != "" && !local {
		return remoteRestart(app, dockerFlag, jsonMode)
	}

	r := selectRunner(app, dockerFlag)
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

func remoteRestart(app *App, dockerFlag, jsonMode bool) error {
	if err := syncAppEnvFile(app.Hive.Server, app); err != nil {
		return err
	}
	downArgs := []string{"--local"}
	if err := runRemoteHive(app.Hive.Server, app.Dir, "down", downArgs, jsonMode); err != nil {
		return fmt.Errorf("remote down: %w", err)
	}
	upArgs := []string{"--local"}
	if dockerFlag {
		upArgs = append(upArgs, "--docker")
	}
	if err := runRemoteHive(app.Hive.Server, app.Dir, "up", upArgs, jsonMode); err != nil {
		return fmt.Errorf("remote up: %w", err)
	}
	return nil
}

func runRemoteHive(server, dir, cmd string, args []string, toStderr bool) error {
	remoteCmd := fmt.Sprintf("cd %s && ~/.local/bin/hive %s", shellSingleQuote(dir), shellJoin(append([]string{cmd}, args...)))
	c := exec.Command("ssh", server, remoteCmd)
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
				return fmt.Errorf("ssh %s ~/.local/bin/hive %s exited %d: %w", server, cmd, status.ExitStatus(), err)
			}
		}
		return fmt.Errorf("ssh %s ~/.local/bin/hive %s failed: %w (is hive installed on the box?)", server, cmd, err)
	}
	return nil
}
