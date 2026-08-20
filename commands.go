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

func cmdDeploy(ctx context.Context, args []string) error {
	var packagesFlags stringSlice
	args = normalizeFlags(args, map[string]bool{"filter": true, "packages": true})
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	dockerFlag := fs.Bool("docker", false, "")
	localFlag := fs.Bool("local", false, "")
	filterFlag := fs.String("filter", "", "")
	fs.Var(&packagesFlags, "packages", "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	target := fs.Arg(0)
	if target != "" && target != "all" {
		return fmt.Errorf("unknown deploy target %q (did you mean \"all\"?)", target)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	if target == "all" || *filterFlag != "" {
		root, ok := findWorkspaceRoot(cwd)
		if !ok {
			if len(packagesFlags) > 0 {
				root = cwd
			} else {
				return fmt.Errorf("not in a workspace; use --packages or run from a workspace root")
			}
		}
		apps, err := discoverWorkspaceApps(root, packagesFlags)
		if err != nil {
			return err
		}
		if len(apps) == 0 {
			return fmt.Errorf("no hive apps found in workspace")
		}
		if *filterFlag != "" {
			app, err := filterApps(apps, *filterFlag)
			if err != nil {
				return err
			}
			return runDeployAndPrint(ctx, app, *dockerFlag, *localFlag, *jsonFlag)
		}
		return deployAll(ctx, apps, *dockerFlag, *localFlag, *jsonFlag)
	}

	app, err := loadCwdApp()
	if err != nil {
		return fmt.Errorf("not in a hive app; use `hive deploy all`, `hive deploy --filter <name>`, or cd into an app")
	}
	return runDeployAndPrint(ctx, app, *dockerFlag, *localFlag, *jsonFlag)
}

func deployAll(ctx context.Context, apps []*App, dockerFlag, localFlag, jsonFlag bool) error {
	var results []deployResult
	var failed int
	start := time.Now()
	for _, app := range apps {
		res := runDeploy(ctx, app, dockerFlag, localFlag, jsonFlag)
		results = append(results, res)
		if res.Error != "" {
			failed++
		}
	}
	if jsonFlag {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal results: %w", err)
		}
		fmt.Println(string(b))
	} else {
		for _, res := range results {
			if res.Error != "" {
				fmt.Printf("%s: failed: %s\n", res.App, res.Error)
			} else {
				fmt.Printf("%s: deployed %s (%dms)\n", res.App, res.Version, res.DurationMs)
			}
		}
		fmt.Printf("deployed %d/%d apps (%dms)\n", len(apps)-failed, len(apps), time.Since(start).Milliseconds())
	}
	if failed > 0 {
		return fmt.Errorf("%d app(s) failed to deploy", failed)
	}
	return nil
}

func runDeployAndPrint(ctx context.Context, app *App, dockerFlag, localFlag, jsonFlag bool) error {
	res := runDeploy(ctx, app, dockerFlag, localFlag, jsonFlag)
	if jsonFlag {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
	} else {
		for _, s := range res.Steps {
			if s.Skipped {
				fmt.Printf("%s skipped\n", s.Name)
			} else if s.OK {
				fmt.Printf("%s ok (%dms)\n", s.Name, s.DurationMs)
			} else {
				fmt.Printf("%s failed (%dms)\n", s.Name, s.DurationMs)
			}
		}
		if res.Version != "" {
			fmt.Printf("version: %s\n", res.Version)
		}
		if res.Error != "" {
			return fmt.Errorf("%s", res.Error)
		}
		if res.Version != "" {
			fmt.Printf("deployed %s (%s) in %dms\n", res.App, res.Version, res.DurationMs)
		} else {
			fmt.Printf("deployed %s in %dms\n", res.App, res.DurationMs)
		}
	}
	if res.Error != "" {
		return fmt.Errorf("%s", res.Error)
	}
	return nil
}

func runDeploy(ctx context.Context, app *App, dockerFlag, localFlag, jsonFlag bool) deployResult {
	start := time.Now()
	var steps []deployStep
	var version string

	st := startStep("typecheck")
	skipped, err := runTypecheck(ctx, app, jsonFlag)
	if err != nil {
		steps = append(steps, st.done(false))
		return deployResult{App: app.Name, Version: version, Steps: steps, DurationMs: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	steps = append(steps, st.doneSkip(skipped))

	st = startStep("deploy")
	version, err = runCelldDeploy(ctx, app, jsonFlag)
	if err != nil {
		steps = append(steps, st.done(false))
		return deployResult{App: app.Name, Version: version, Steps: steps, DurationMs: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	steps = append(steps, st.done(true))

	st = startStep("restart")
	if err := restartNode(ctx, app, dockerFlag, localFlag, jsonFlag); err != nil {
		steps = append(steps, st.done(false))
		return deployResult{App: app.Name, Version: version, Steps: steps, DurationMs: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	steps = append(steps, st.done(true))

	st = startStep("health")
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if ok, msg := waitForHealth(healthCtx, app); !ok {
		logPath := filepath.Join(app.Dir, ".hive", "node.log")
		steps = append(steps, st.done(false))
		return deployResult{App: app.Name, Version: version, Steps: steps, DurationMs: time.Since(start).Milliseconds(), Error: fmt.Sprintf("health gate timed out after 30s (see %s): %s", logPath, msg)}
	}
	steps = append(steps, st.done(true))

	return deployResult{App: app.Name, Version: version, Steps: steps, DurationMs: time.Since(start).Milliseconds()}
}


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

// gatherStatus inspects the local node state for app, returning the same
// structure used by `hive status --json`.
func gatherStatus(ctx context.Context, app *App) (statusResult, error) {
	var st nodeStatus
	var err error
	for _, r := range []runner{dockerRunner{}, processRunner{}} {
		st, err = r.Status(ctx, app)
		if err == nil && (st.Running || st.PID != 0) {
			break
		}
	}
	if err != nil {
		return statusResult{}, err
	}
	st.Version = fetchLiveVersion(app)
	return statusResult{App: *app, Server: app.Hive.Server, Node: st}, nil
}

func cmdStatus(ctx context.Context, args []string) error {
	var packagesFlags stringSlice
	args = normalizeFlags(args, map[string]bool{"filter": true, "packages": true})
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	localFlag := fs.Bool("local", false, "")
	filterFlag := fs.String("filter", "", "")
	fs.Var(&packagesFlags, "packages", "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	if *filterFlag != "" {
		app, err := resolveWorkspaceApp(cwd, *filterFlag, packagesFlags)
		if err != nil {
			return err
		}
		return printSingleStatus(ctx, app, *localFlag, *jsonFlag)
	}

	app, appErr := loadCwdApp()
	if appErr == nil {
		return printSingleStatus(ctx, app, *localFlag, *jsonFlag)
	}

	root, ok := findWorkspaceRoot(cwd)
	if !ok {
		if len(packagesFlags) > 0 {
			root = cwd
		} else {
			return appErr
		}
	}
	apps, err := discoverWorkspaceApps(root, packagesFlags)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return fmt.Errorf("no hive apps found in workspace")
	}
	return printFleetStatus(ctx, apps, *localFlag, *jsonFlag)
}

func resolveWorkspaceApp(cwd, filter string, packagesFlags []string) (*App, error) {
	root, ok := findWorkspaceRoot(cwd)
	if !ok {
		if len(packagesFlags) > 0 {
			root = cwd
		} else {
			return nil, fmt.Errorf("not in a workspace; use --packages or run from a workspace root")
		}
	}
	apps, err := discoverWorkspaceApps(root, packagesFlags)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("no hive apps found in workspace")
	}
	return filterApps(apps, filter)
}

func printSingleStatus(ctx context.Context, app *App, local, jsonFlag bool) error {
	if app.Hive.Server != "" && !local {
		return remoteHive(app.Hive.Server, app.Dir, "status", []string{"--local"})
	}

	res, err := gatherStatus(ctx, app)
	if err != nil {
		return err
	}
	if jsonFlag {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal status: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	st := res.Node

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

func printFleetStatus(ctx context.Context, apps []*App, local, jsonFlag bool) error {
	var results []statusResult
	for _, app := range apps {
		results = append(results, gatherStatusForFleet(ctx, app, local))
	}
	if jsonFlag {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal fleet status: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}

	fmt.Printf("%-12s %-6s %-18s %-10s %-10s %s\n", "NAME", "PORT", "SERVER/BACKEND", "NODE", "HEALTH", "VERSION")
	for _, res := range results {
		app := res.App
		st := res.Node
		serverBackend := "-"
		if app.Hive.Server != "" || st.Backend != "" {
			serverBackend = fmt.Sprintf("%s/%s", firstNonEmpty(app.Hive.Server, "-"), st.Backend)
		}
		node := map[bool]string{true: "running", false: "down"}[st.Running]
		health := "-"
		if st.Running {
			health = map[bool]string{true: "ok", false: "unhealthy"}[st.Healthy]
		}
		version := st.Version
		if version == "" {
			version = "-"
		}
		fmt.Printf("%-12s %-6d %-18s %-10s %-10s %s\n", app.Name, app.Hive.Port, serverBackend, node, health, version)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func gatherStatusForFleet(ctx context.Context, app *App, local bool) statusResult {
	if app.Hive.Server != "" && !local {
		res, err := remoteStatusJSON(app.Hive.Server, app.Dir)
		if err == nil {
			return res
		}
		return statusResult{App: *app, Server: app.Hive.Server, Node: nodeStatus{Healthy: false, HealthError: err.Error()}}
	}
	res, err := gatherStatus(ctx, app)
	if err != nil {
		return statusResult{App: *app, Server: app.Hive.Server, Node: nodeStatus{Healthy: false, HealthError: err.Error()}}
	}
	return res
}

func remoteStatusJSON(server, dir string) (statusResult, error) {
	remoteCmd := fmt.Sprintf("cd %s && ~/.local/bin/hive status --local --json", shellSingleQuote(dir))
	cmd := exec.Command("ssh", server, remoteCmd)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return statusResult{}, fmt.Errorf("ssh status: %s", msg)
		}
		return statusResult{}, fmt.Errorf("ssh status: %w", err)
	}
	var res statusResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return statusResult{}, fmt.Errorf("parse remote status: %w", err)
	}
	return res, nil
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
