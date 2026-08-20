package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// nodeStatus is what every run backend returns about an app's node.
type nodeStatus struct {
	Running     bool   `json:"running"`
	Backend     string `json:"backend"`
	PID         int    `json:"pid,omitempty"`
	Healthy     bool   `json:"healthy"`
	HealthError string `json:"health_error,omitempty"`
	Version     string `json:"version,omitempty"`
}

// runner starts, stops, and introspects a celld node for one app.
type runner interface {
	Name() string
	Up(ctx context.Context, app *App) error
	Down(ctx context.Context, app *App) error
	Status(ctx context.Context, app *App) (nodeStatus, error)
}

// selectRunner picks the node backend: docker when asked (flag or hive
// block), otherwise a plain local process.
func selectRunner(app *App, dockerFlag bool) runner {
	if useDockerBackend(app, dockerFlag) {
		return dockerRunner{}
	}
	return processRunner{}
}

func celldPath() (string, error) {
	if p, err := exec.LookPath("celld"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find celld binary: %w", err)
	}
	p := filepath.Join(home, ".local", "bin", "celld")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("celld binary not found on PATH or at ~/.local/bin/celld")
}

func celldBucketEnv() (string, error) {
	b := os.Getenv("CELLD_BUCKET")
	if b == "" {
		return "", fmt.Errorf("CELLD_BUCKET is not set (export it, or run `hive init`)")
	}
	return strings.TrimPrefix(b, "s3://"), nil
}

func celldArgs(app *App) ([]string, error) {
	bucket, err := celldBucketEnv()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--bucket", bucket + "/" + app.Name,
		"--listen", fmt.Sprintf("127.0.0.1:%d", app.Hive.Port),
		"--trust-forwarded-headers",
	}
	if ep := os.Getenv("S3_ENDPOINT"); ep != "" {
		args = append(args, "--endpoint", ep)
	}
	if r := os.Getenv("AWS_REGION"); r != "" {
		args = append(args, "--region", r)
	}
	return args, nil
}

func celldEnviron() []string {
	keep := []string{
		"PATH", "HOME", "USER", "SHELL", "TMPDIR",
		"CELLD_BUCKET", "CELLD_WATCH", "CELLD_ESBUILD",
		"S3_ENDPOINT", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"RUST_LOG",
	}
	out := make([]string, 0, len(keep))
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func healthURL(app *App) string {
	return fmt.Sprintf("http://127.0.0.1:%d/__celld/health", app.Hive.Port)
}

func healthCheck(ctx context.Context, app *App) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL(app), nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("http %d: %s", resp.StatusCode, string(body))
	}
	var v struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false, fmt.Sprintf("invalid health body: %s", string(body))
	}
	if !v.OK {
		return false, string(body)
	}
	return true, ""
}

func waitForHealth(ctx context.Context, app *App) (bool, string) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	for time.Now().Before(deadline) {
		if ok, err := healthCheck(ctx, app); ok {
			return true, ""
		} else if err != "" {
			// keep retrying until deadline
		}
		time.Sleep(200 * time.Millisecond)
	}
	ok, err := healthCheck(ctx, app)
	return ok, err
}

func waitForPortClosed(ctx context.Context, port int) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		c, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil
		}
		c.Close()
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %d is still listening", port)
}

func pidOwningPort(port int) (int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("lsof", "-ti", fmt.Sprintf("tcp:%d", port))
	} else {
		cmd = exec.Command("fuser", "-n", "tcp", strconv.Itoa(port))
	}
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("no process owns port %d: %w", port, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("no process owns port %d", port)
	}
	// fuser prints "<port>/tcp: <pids>"; lsof -ti prints one pid per line.
	for _, f := range strings.Fields(s) {
		f = strings.TrimSuffix(f, "/tcp")
		f = strings.TrimSuffix(f, ":")
		if pid, err := strconv.Atoi(f); err == nil {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no process owns port %d", port)
}

func signalPID(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := p.Signal(sig); err != nil {
		return fmt.Errorf("signal %d: %w", pid, err)
	}
	return nil
}

func isPortListening(port int) bool {
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	c, err := dialer.DialContext(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	c.Close()
	return true
}
