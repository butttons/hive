package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dockerRunner runs the node as a container from the official celld image,
// published on 127.0.0.1:<port>, restarted by docker. Same deployment
// targets as celld itself: linux/amd64, linux/arm64, darwin/arm64 (via
// Docker Desktop's VM).
type dockerRunner struct{}

const dockerImageRepo = "ghcr.io/denoland/celld"

func dockerContainerName(app *App) string { return "hive-" + app.Name }

func docker(args ...string) (string, error) {
	c := exec.Command("docker", args...)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// celldVersion reads the local celld binary's version, used to pin the
// image to the same release the operator deploys with.
func celldVersion() (string, error) {
	if v := os.Getenv("HIVE_CELLD_VERSION"); v != "" {
		return v, nil
	}
	bin, err := celldPath()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("celld --version: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("cannot parse celld version from %q", strings.TrimSpace(string(out)))
	}
	v := fields[len(fields)-1]
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v, nil
}

// ensureDockerImage pulls ghcr.io/denoland/celld:<version> if missing.
func ensureDockerImage(ctx context.Context) (string, error) {
	version, err := celldVersion()
	if err != nil {
		return "", err
	}
	image := dockerImageRepo + ":" + version
	if _, err := docker("image", "inspect", image); err == nil {
		return image, nil
	}
	fmt.Printf("pulling image %s\n", image)
	c := exec.CommandContext(ctx, "docker", "pull", image)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("docker pull %s: %w", image, err)
	}
	return image, nil
}

// dockerEnvSpec is the desired container configuration; its hash is stored
// as a label so drift triggers a recreate.
func dockerEnvSpec(app *App, image string) ([]string, string, error) {
	bucket, err := celldBucketEnv()
	if err != nil {
		return nil, "", err
	}
	env := map[string]string{
		"CELLD_BUCKET":        bucket + "/" + app.Name,
		"CELLD_ADDR":          "0.0.0.0:8080",
		"CELLD_INTERNAL_ADDR": "127.0.0.1:8081",
		"CELLD_WATCH":         "/data",
	}
	for _, k := range []string{"S3_ENDPOINT", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "RUST_LOG"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	for _, k := range []string{"S3_ENDPOINT", "AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if env[k] == "" {
			return nil, "", fmt.Errorf("%s is not set (export it, or run hive init)", k)
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	h := sha256.New()
	h.Write([]byte(image))
	fmt.Fprintf(h, "%d", app.Hive.Port)
	for _, k := range keys {
		lines = append(lines, k+"="+env[k])
		fmt.Fprintf(h, "%s=%s", k, env[k])
	}
	return lines, hex.EncodeToString(h.Sum(nil)), nil
}

func dockerInspect(ctx context.Context, app *App) (running bool, configHash string, exists bool, err error) {
	out, derr := docker("inspect", "-f", "{{.State.Running}} {{index .Config.Labels \"hive.config\"}}", dockerContainerName(app))
	if derr != nil {
		return false, "", false, nil
	}
	parts := strings.Fields(out)
	if len(parts) == 2 {
		return parts[0] == "true", parts[1], true, nil
	}
	return parts[0] == "true", "", true, nil
}

func (dockerRunner) Name() string { return "docker" }

func (dockerRunner) Up(ctx context.Context, app *App) error {
	image, err := ensureDockerImage(ctx)
	if err != nil {
		return err
	}
	envLines, hash, err := dockerEnvSpec(app, image)
	if err != nil {
		return err
	}

	running, existingHash, exists, err := dockerInspect(ctx, app)
	if err != nil {
		return err
	}
	if exists && running && existingHash == hash {
		healthy, _ := healthCheck(ctx, app)
		if healthy {
			fmt.Printf("node for %s is already up (docker, healthy)\n", app.Name)
			return nil
		}
	}
	if exists {
		if _, err := docker("rm", "-f", dockerContainerName(app)); err != nil {
			return fmt.Errorf("remove stale container: %w", err)
		}
	}

	envFile := filepath.Join(appEnvDir(), app.Name+".dockerenv")
	if err := os.MkdirAll(appEnvDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(envFile, []byte(strings.Join(envLines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", envFile, err)
	}

	args := []string{
		"run", "-d",
		"--name", dockerContainerName(app),
		"--restart", "unless-stopped",
		"--label", "hive.app=" + app.Name,
		"--label", "hive.config=" + hash,
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", app.Hive.Port),
		"-v", dockerContainerName(app) + "-data:/data",
		"--env-file", envFile,
		image,
		"--trust-forwarded-headers",
	}
	if out, err := docker(args...); err != nil {
		return fmt.Errorf("docker run: %s: %w", out, err)
	}
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if ok, msg := waitForHealth(healthCtx, app); !ok {
		logs, _ := docker("logs", "--tail", "20", dockerContainerName(app))
		return fmt.Errorf("node did not become healthy: %s\ncontainer logs:\n%s", msg, logs)
	}
	fmt.Printf("node for %s is up (docker)\n", app.Name)
	return nil
}

func (dockerRunner) Down(ctx context.Context, app *App) error {
	_, _, exists, err := dockerInspect(ctx, app)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Printf("no docker container for %s\n", app.Name)
		return nil
	}
	// docker stop sends SIGTERM; celld drains gracefully.
	if _, err := docker("stop", dockerContainerName(app)); err != nil {
		return fmt.Errorf("docker stop: %w", err)
	}
	if _, err := docker("rm", dockerContainerName(app)); err != nil {
		return fmt.Errorf("docker rm: %w", err)
	}
	fmt.Printf("node for %s stopped\n", app.Name)
	return nil
}

func (dockerRunner) Status(ctx context.Context, app *App) (nodeStatus, error) {
	st := nodeStatus{Backend: "docker"}
	running, _, exists, err := dockerInspect(ctx, app)
	if err != nil {
		return st, err
	}
	if exists {
		st.Running = running
	}
	healthy, msg := healthCheck(ctx, app)
	st.Healthy = healthy
	if !healthy {
		st.HealthError = msg
	}
	return st, nil
}

// useDockerBackend reports whether the operator picked the docker backend
// via --docker or "backend": "docker" in the hive block.
func useDockerBackend(app *App, dockerFlag bool) bool {
	return dockerFlag || app.Hive.Backend == "docker"
}
