package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type systemdRunner struct{}

func (systemdRunner) Name() string { return "systemd" }

func (systemdRunner) Up(ctx context.Context, app *App) error {
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

	wrapperPath := filepath.Join(hiveDir, "run.sh")
	oldWrapper, _ := os.ReadFile(wrapperPath)
	wrapperPath, err = writeRunWrapper(app, bin, args)
	if err != nil {
		return fmt.Errorf("write run wrapper: %w", err)
	}
	newWrapper, _ := os.ReadFile(wrapperPath)

	unit, err := systemdUnit(app, wrapperPath, watchDir)
	if err != nil {
		return fmt.Errorf("generate unit: %w", err)
	}
	path := systemdUnitPath(app)
	needsStart := true
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, unit) && bytes.Equal(oldWrapper, newWrapper) {
		active, _ := systemdIsActive(app)
		if active {
			st, _ := systemdStatus(ctx, app)
			if st.Healthy {
				fmt.Printf("node for %s is already up (systemd, healthy)\n", app.Name)
				return nil
			}
			needsStart = false
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create systemd user dir: %w", err)
	}
	if err := os.WriteFile(path, unit, 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := systemdDaemonReload(); err != nil {
		return err
	}
	if needsStart {
		if err := systemdStart(app); err != nil {
			return err
		}
	} else {
		if err := systemdRestart(app); err != nil {
			return err
		}
	}
	if ok, msg := waitForHealth(ctx, app); !ok {
		return fmt.Errorf("node did not become healthy: %s", msg)
	}
	fmt.Printf("node for %s is up (systemd)\n", app.Name)
	return nil
}

func (systemdRunner) Down(ctx context.Context, app *App) error {
	active, _ := systemdIsActive(app)
	if !active {
		fmt.Printf("node for %s is already down\n", app.Name)
		return nil
	}
	if err := systemdStop(app); err != nil {
		return err
	}
	if err := waitForPortClosed(ctx, app.Hive.Port); err != nil {
		return fmt.Errorf("wait for node to stop: %w", err)
	}
	fmt.Printf("node for %s stopped\n", app.Name)
	return nil
}

func (systemdRunner) Status(ctx context.Context, app *App) (nodeStatus, error) {
	return systemdStatus(ctx, app)
}

func systemdUnitName(app *App) string {
	return "dev.celld." + app.Name + ".service"
}

func systemdUnitPath(app *App) string {
	cfg, _ := os.UserConfigDir()
	return filepath.Join(cfg, "systemd", "user", systemdUnitName(app))
}

func systemdIsActive(app *App) (bool, error) {
	cmd := exec.Command("systemctl", "--user", "is-active", systemdUnitName(app))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("systemctl is-active: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)) == "active", nil
}

func systemdStart(app *App) error {
	cmd := exec.Command("systemctl", "--user", "start", systemdUnitName(app))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl start: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func systemdStop(app *App) error {
	cmd := exec.Command("systemctl", "--user", "stop", systemdUnitName(app))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl stop: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func systemdRestart(app *App) error {
	cmd := exec.Command("systemctl", "--user", "restart", systemdUnitName(app))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func systemdDaemonReload() error {
	cmd := exec.Command("systemctl", "--user", "daemon-reload")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl daemon-reload: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func systemdStatus(ctx context.Context, app *App) (nodeStatus, error) {
	st := nodeStatus{Backend: "systemd"}
	active, _ := systemdIsActive(app)
	if !active {
		return st, nil
	}
	st.Running = isPortListening(app.Hive.Port)
	st.PID, _ = pidOwningPort(app.Hive.Port)
	ok, msg := healthCheck(ctx, app)
	st.Healthy = ok
	st.HealthError = msg
	return st, nil
}

func systemdUnit(app *App, wrapperPath string, watchDir string) ([]byte, error) {
	hiveDir := filepath.Join(app.Dir, ".hive")
	var buf bytes.Buffer
	buf.WriteString("[Unit]\n")
	fmt.Fprintf(&buf, "Description=celld node for %s\n", app.Name)
	buf.WriteString("After=network.target\n\n")
	buf.WriteString("[Service]\n")
	fmt.Fprintf(&buf, "Type=simple\n")
	fmt.Fprintf(&buf, "WorkingDirectory=%s\n", app.Dir)
	fmt.Fprintf(&buf, "ExecStart=%s\n", wrapperPath)
	fmt.Fprintf(&buf, "Restart=always\n")
	fmt.Fprintf(&buf, "RestartSec=1\n")

	fmt.Fprintf(&buf, "StandardOutput=append:%s\n", filepath.Join(hiveDir, "node.log"))
	fmt.Fprintf(&buf, "StandardError=append:%s\n", filepath.Join(hiveDir, "node.log"))
	buf.WriteString("\n[Install]\n")
	buf.WriteString("WantedBy=default.target\n")
	return buf.Bytes(), nil
}

