package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

type processRunner struct{}

func (processRunner) Name() string { return "process" }

func (processRunner) Up(ctx context.Context, app *App) error {
	st, err := processStatus(ctx, app)
	if err != nil {
		return err
	}
	if st.Running && st.Healthy {
		fmt.Printf("node for %s is already up (pid %d, healthy)\n", app.Name, st.PID)
		return nil
	}
	if st.Running && st.PID != 0 {
		if err := signalPID(st.PID, syscall.SIGTERM); err != nil {
			return fmt.Errorf("stop unhealthy node: %w", err)
		}
		if err := waitForPortClosed(ctx, app.Hive.Port); err != nil {
			return fmt.Errorf("wait for node to stop: %w", err)
		}
	}

	bin, err := celldPath()
	if err != nil {
		return err
	}
	args, err := celldArgs(app)
	if err != nil {
		return err
	}

	hiveDir := filepath.Join(app.Dir, ".hive")
	if err := os.MkdirAll(hiveDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", hiveDir, err)
	}
	watchDir := filepath.Join(hiveDir, "watch")
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		return fmt.Errorf("create watch dir: %w", err)
	}
	logPath := filepath.Join(hiveDir, "node.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	env := append(os.Environ(), celldEnviron()...)
	env = append(env, "CELLD_WATCH="+watchDir)

	cmd := exec.Command(bin, args...)
	cmd.Dir = app.Dir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start celld: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release celld process: %w", err)
	}

	if ok, msg := waitForHealth(ctx, app); !ok {
		return fmt.Errorf("node did not become healthy: %s", msg)
	}
	fmt.Printf("node for %s is up (process)\n", app.Name)
	return nil
}

func (processRunner) Down(ctx context.Context, app *App) error {
	st, err := processStatus(ctx, app)
	if err != nil {
		return err
	}
	if !st.Running {
		fmt.Printf("node for %s is already down\n", app.Name)
		return nil
	}
	if st.PID == 0 {
		return fmt.Errorf("node is running but no pid found")
	}
	if err := signalPID(st.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop node: %w", err)
	}
	if err := waitForPortClosed(ctx, app.Hive.Port); err != nil {
		return fmt.Errorf("wait for node to stop: %w", err)
	}
	fmt.Printf("node for %s stopped\n", app.Name)
	return nil
}

func (processRunner) Status(ctx context.Context, app *App) (nodeStatus, error) {
	return processStatus(ctx, app)
}

func processStatus(ctx context.Context, app *App) (nodeStatus, error) {
	st := nodeStatus{Backend: "process"}
	if !isPortListening(app.Hive.Port) {
		return st, nil
	}
	st.Running = true
	st.PID, _ = pidOwningPort(app.Hive.Port)
	ok, msg := healthCheck(ctx, app)
	st.Healthy = ok
	st.HealthError = msg
	return st, nil
}
