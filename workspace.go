package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// stringSlice is a flag.Value that accepts the flag multiple times or as a
// comma-separated list.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// findWorkspaceRoot walks up from dir looking for a pnpm-workspace file or a
// root package.json with a workspaces field. It stops at the git root or the
// filesystem root.
func findWorkspaceRoot(from string) (string, bool) {
	dir := from
	for {
		if hasPnpmWorkspace(dir) {
			return dir, true
		}
		if hasPackageWorkspaces(filepath.Join(dir, "package.json")) {
			return dir, true
		}
		if isGitRoot(dir) {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func hasPnpmWorkspace(dir string) bool {
	for _, name := range []string{"pnpm-workspace.yml", "pnpm-workspace.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func hasPackageWorkspaces(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var pkg struct {
		Workspaces any `json:"workspaces"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return false
	}
	switch v := pkg.Workspaces.(type) {
	case []any:
		return len(v) > 0
	case map[string]any:
		if arr, ok := v["packages"].([]any); ok {
			return len(arr) > 0
		}
	}
	return false
}

func isGitRoot(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

// loadWorkspacePatterns returns the package glob patterns declared by the
// workspace at root. pnpm-workspace files take precedence over npm/yarn
// workspaces in package.json.
func loadWorkspacePatterns(root string) ([]string, error) {
	for _, name := range []string{"pnpm-workspace.yml", "pnpm-workspace.yaml"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			return parsePnpmWorkspace(path)
		}
	}
	return parseRootPackageWorkspaces(filepath.Join(root, "package.json"))
}

func parsePnpmWorkspace(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var patterns []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			item := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			item = strings.Trim(item, `"'`)
			if item != "" {
				patterns = append(patterns, item)
			}
		}
	}
	return patterns, nil
}

func parseRootPackageWorkspaces(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pkg struct {
		Workspaces any `json:"workspaces"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch v := pkg.Workspaces.(type) {
	case []any:
		return stringsFromAny(v), nil
	case map[string]any:
		if arr, ok := v["packages"].([]any); ok {
			return stringsFromAny(arr), nil
		}
	}
	return nil, nil
}

func stringsFromAny(v []any) []string {
	var out []string
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// discoverWorkspaceApps enumerates hive apps under root using the workspace's
// declared patterns, or extraPatterns when no workspace file exists.
func discoverWorkspaceApps(root string, extraPatterns []string) ([]*App, error) {
	patterns, err := loadWorkspacePatterns(root)
	if err != nil {
		return nil, err
	}
	if len(extraPatterns) > 0 {
		patterns = extraPatterns
	}
	if len(patterns) == 0 {
		return nil, nil
	}

	var apps []*App
	seen := map[string]bool{}
	for _, pat := range patterns {
		dirs, err := expandPattern(root, pat)
		if err != nil {
			return nil, fmt.Errorf("expand %q: %w", pat, err)
		}
		for _, d := range dirs {
			if seen[d] {
				continue
			}
			seen[d] = true
			app, err := LoadApp(d)
			if err != nil {
				continue
			}
			if err := loadAppEnv(app); err != nil {
				continue
			}
			apps = append(apps, app)
		}
	}
	slices.SortFunc(apps, func(a, b *App) int {
		return strings.Compare(a.Name, b.Name)
	})
	return apps, nil
}

// expandPattern implements minimal glob expansion for workspace patterns. Only
// a single trailing "/*" is supported; literal directory paths pass through.
func expandPattern(root, pat string) ([]string, error) {
	pat = strings.TrimPrefix(pat, "./")
	if !strings.Contains(pat, "*") {
		full := filepath.Join(root, pat)
		info, err := os.Stat(full)
		if err == nil && info.IsDir() {
			return []string{full}, nil
		}
		return nil, nil
	}
	if strings.HasSuffix(pat, "/*") {
		prefix := filepath.Join(root, strings.TrimSuffix(pat, "/*"))
		entries, err := os.ReadDir(prefix)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		var dirs []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
				continue
			}
			dirs = append(dirs, filepath.Join(prefix, e.Name()))
		}
		return dirs, nil
	}
	return nil, nil
}

// filterApps selects the single app whose package.json name or directory
// basename matches filter. The match is case-insensitive.
func filterApps(apps []*App, filter string) (*App, error) {
	filter = strings.ToLower(filter)
	var matches []*App
	for _, a := range apps {
		if strings.ToLower(a.Name) == filter || strings.ToLower(filepath.Base(a.Dir)) == filter {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("filter %q matches multiple apps: %s", filter, appNames(matches))
	}
	return nil, fmt.Errorf("no app matches %q (available: %s)", filter, appNames(apps))
}

func appNames(apps []*App) string {
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}
