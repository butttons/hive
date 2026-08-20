package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type launchdRunner struct{}

func (launchdRunner) Name() string { return "launchd" }

func (launchdRunner) Up(ctx context.Context, app *App) error {
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
	plist, err := launchdPlist(app, wrapperPath, watchDir)
	if err != nil {
		return fmt.Errorf("generate plist: %w", err)
	}
	path := launchdPlistPath(app)

	needsBootstrap := true
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, plist) && bytes.Equal(oldWrapper, newWrapper) {
		loaded, _ := launchdList(app)
		if loaded {
			st, _ := launchdStatus(ctx, app)
			if st.Healthy {
				fmt.Printf("node for %s is already up (launchd, healthy)\n", app.Name)
				return nil
			}
			if err := launchdBootout(app); err != nil {
				return fmt.Errorf("stop unhealthy launchd job: %w", err)
			}
		}
		needsBootstrap = true
	} else if loaded, _ := launchdList(app); loaded {
		if err := launchdBootout(app); err != nil {
			return fmt.Errorf("stop old launchd job: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(path, plist, 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if needsBootstrap {
		if err := launchdBootstrap(app, path); err != nil {
			return fmt.Errorf("bootstrap launchd job: %w", err)
		}
		if ok, msg := waitForHealth(ctx, app); !ok {
			return fmt.Errorf("node did not become healthy: %s", msg)
		}
		fmt.Printf("node for %s is up (launchd)\n", app.Name)
	}
	return nil
}

func (launchdRunner) Down(ctx context.Context, app *App) error {
	loaded, _ := launchdList(app)
	if !loaded {
		fmt.Printf("node for %s is already down\n", app.Name)
		return nil
	}
	if err := launchdBootout(app); err != nil {
		return fmt.Errorf("stop launchd job: %w", err)
	}
	if err := waitForPortClosed(ctx, app.Hive.Port); err != nil {
		return fmt.Errorf("wait for node to stop: %w", err)
	}
	fmt.Printf("node for %s stopped\n", app.Name)
	return nil
}

func (launchdRunner) Status(ctx context.Context, app *App) (nodeStatus, error) {
	return launchdStatus(ctx, app)
}

func launchdLabel(app *App) string {
	return "dev.celld." + app.Name
}

func launchdPlistPath(app *App) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel(app)+".plist")
}

func launchdDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchdBootstrap(app *App, path string) error {
	cmd := exec.Command("launchctl", "bootstrap", launchdDomain(), path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// Bootstrap from a non-GUI session (e.g. SSH) loads the job but does not
	// always start it. Kickstart ensures it runs before we wait for health.
	cmd = exec.Command("launchctl", "kickstart", launchdDomain()+"/"+launchdLabel(app))
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func launchdBootout(app *App) error {
	target := launchdDomain() + "/" + launchdLabel(app)
	cmd := exec.Command("launchctl", "bootout", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout %s: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func launchdList(app *App) (bool, error) {
	cmd := exec.Command("launchctl", "list", launchdLabel(app))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("launchctl list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return true, nil
}

func launchdStatus(ctx context.Context, app *App) (nodeStatus, error) {
	st := nodeStatus{Backend: "launchd"}
	loaded, _ := launchdList(app)
	if !loaded {
		return st, nil
	}
	st.Running = isPortListening(app.Hive.Port)
	st.PID, _ = pidOwningPort(app.Hive.Port)
	ok, msg := healthCheck(ctx, app)
	st.Healthy = ok
	st.HealthError = msg
	return st, nil
}

func launchdPlist(app *App, wrapperPath string, watchDir string) ([]byte, error) {
	hiveDir := filepath.Join(app.Dir, ".hive")

	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	buf.WriteString(`<plist version="1.0">` + "\n")
	buf.WriteString(`<dict>` + "\n")
	writePlistString(&buf, "Label", launchdLabel(app))
	writePlistString(&buf, "WorkingDirectory", app.Dir)
	writePlistString(&buf, "StandardOutPath", filepath.Join(hiveDir, "node.log"))
	writePlistString(&buf, "StandardErrorPath", filepath.Join(hiveDir, "node.log"))

	buf.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	buf.WriteString("\t\t<string>/bin/sh</string>\n")
	buf.WriteString("\t\t<string>" + plistEscape(wrapperPath) + "</string>\n")
	buf.WriteString("\t</array>\n")

	buf.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	buf.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	buf.WriteString(`</dict>` + "\n")
	buf.WriteString(`</plist>` + "\n")
	return buf.Bytes(), nil
}

func writePlistString(buf *bytes.Buffer, key, value string) {
	buf.WriteString("\t<key>" + plistEscape(key) + "</key>\n")
	buf.WriteString("\t<string>" + plistEscape(value) + "</string>\n")
}

func plistEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
