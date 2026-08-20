package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeApp(t *testing.T, dir, name string, port int) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"`+name+`","hive":{"port":`+itoa(port)+`}}`)
	writeFile(t, filepath.Join(dir, "wrangler.jsonc"), `{"name":"`+name+`","main":"index.ts"}`)
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestFindWorkspaceRootPnpm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - \"apps/*\"\n")
	writeFile(t, filepath.Join(root, "package.json"), `{}`)

	cwd := filepath.Join(root, "apps", "counter")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findWorkspaceRoot(cwd)
	if !ok || got != root {
		t.Fatalf("findWorkspaceRoot = %q, %v; want %q, true", got, ok, root)
	}
}

func TestFindWorkspaceRootStopsAtGitRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - \"apps/*\"\n")
	writeFile(t, filepath.Join(root, ".git"), ``) // file, not dir

	cwd := filepath.Join(root, "apps", "counter")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findWorkspaceRoot(cwd)
	if !ok || got != root {
		t.Fatalf("findWorkspaceRoot = %q, %v; want %q, true", got, ok, root)
	}
}

func TestFindWorkspaceRootPackageJSONArray(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"workspaces": ["packages/*"]}`)

	cwd := filepath.Join(root, "packages", "foo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findWorkspaceRoot(cwd)
	if !ok || got != root {
		t.Fatalf("findWorkspaceRoot = %q, %v; want %q, true", got, ok, root)
	}
}

func TestFindWorkspaceRootPackageJSONObject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"workspaces": {"packages": ["apps/*"]}}`)

	cwd := filepath.Join(root, "apps", "foo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findWorkspaceRoot(cwd)
	if !ok || got != root {
		t.Fatalf("findWorkspaceRoot = %q, %v; want %q, true", got, ok, root)
	}
}

func TestDiscoverWorkspaceAppsPnpm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - \"apps/*\"\n  - \"packages/*\"\n")
	writeApp(t, filepath.Join(root, "apps", "counter"), "counter", 8101)
	writeApp(t, filepath.Join(root, "apps", "wsecho"), "wsecho", 8102)
	writeFile(t, filepath.Join(root, "packages", "lib", "package.json"), `{"name":"lib"}`)

	apps, err := discoverWorkspaceApps(root, nil)
	if err != nil {
		t.Fatalf("discoverWorkspaceApps: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2", len(apps))
	}
	names := []string{apps[0].Name, apps[1].Name}
	if !slices.Equal(names, []string{"counter", "wsecho"}) {
		t.Fatalf("unexpected app order/names: %v", names)
	}
}

func TestDiscoverWorkspaceAppsPackageJSONArray(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"workspaces": ["apps/*"]}`)
	writeApp(t, filepath.Join(root, "apps", "alpha"), "alpha", 8101)

	apps, err := discoverWorkspaceApps(root, nil)
	if err != nil {
		t.Fatalf("discoverWorkspaceApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "alpha" {
		t.Fatalf("got %+v, want [alpha]", apps)
	}
}

func TestDiscoverWorkspaceAppsPackageJSONObject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"workspaces": {"packages": ["apps/*"]}}`)
	writeApp(t, filepath.Join(root, "apps", "beta"), "beta", 8102)

	apps, err := discoverWorkspaceApps(root, nil)
	if err != nil {
		t.Fatalf("discoverWorkspaceApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "beta" {
		t.Fatalf("got %+v, want [beta]", apps)
	}
}

func TestDiscoverWorkspaceAppsFallbackPackages(t *testing.T) {
	root := t.TempDir()
	writeApp(t, filepath.Join(root, "services", "one"), "one", 8101)

	apps, err := discoverWorkspaceApps(root, []string{"services/*"})
	if err != nil {
		t.Fatalf("discoverWorkspaceApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "one" {
		t.Fatalf("got %+v, want [one]", apps)
	}
}

func TestDiscoverWorkspaceAppsSkipsNonHive(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - \"apps/*\"\n")
	writeApp(t, filepath.Join(root, "apps", "hiveapp"), "hiveapp", 8101)
	writeFile(t, filepath.Join(root, "apps", "plain", "package.json"), `{"name":"plain"}`)

	apps, err := discoverWorkspaceApps(root, nil)
	if err != nil {
		t.Fatalf("discoverWorkspaceApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "hiveapp" {
		t.Fatalf("got %+v, want [hiveapp]", apps)
	}
}

func TestFilterAppsByName(t *testing.T) {
	apps := []*App{{Name: "counter", Dir: "/x/apps/counter"}, {Name: "wsecho", Dir: "/x/apps/wsecho"}}
	got, err := filterApps(apps, "counter")
	if err != nil || got.Name != "counter" {
		t.Fatalf("filterApps: %v, %v", got, err)
	}
}

func TestFilterAppsByDirBasename(t *testing.T) {
	apps := []*App{{Name: "counter", Dir: "/x/apps/counter"}, {Name: "wsecho", Dir: "/x/apps/wsecho"}}
	got, err := filterApps(apps, "wsecho")
	if err != nil || got.Name != "wsecho" {
		t.Fatalf("filterApps: %v, %v", got, err)
	}
}

func TestFilterAppsCaseInsensitive(t *testing.T) {
	apps := []*App{{Name: "Counter", Dir: "/x/apps/counter"}}
	got, err := filterApps(apps, "counter")
	if err != nil || got.Name != "Counter" {
		t.Fatalf("filterApps: %v, %v", got, err)
	}
}

func TestFilterAppsNoMatch(t *testing.T) {
	apps := []*App{{Name: "counter", Dir: "/x/apps/counter"}}
	_, err := filterApps(apps, "missing")
	if err == nil {
		t.Fatal("expected error for missing filter")
	}
	if !contains(err.Error(), "available: counter") {
		t.Fatalf("error should list available apps: %v", err)
	}
}

func TestExpandPatternTrailingStar(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apps", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "apps", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "apps", "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}

	dirs, err := expandPattern(root, "apps/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("got %d dirs, want 2: %v", len(dirs), dirs)
	}
}

func TestParsePnpmWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pnpm-workspace.yaml")
	writeFile(t, path, "packages:\n  - \"apps/*\"\n  - 'packages/*'\n")
	got, err := parsePnpmWorkspace(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apps/*", "packages/*"}
	if !slices.Equal(got, want) {
		t.Fatalf("parsePnpmWorkspace = %v, want %v", got, want)
	}
}

func TestParseRootPackageWorkspacesArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	writeFile(t, path, `{"workspaces": ["packages/*"]}`)
	got, err := parseRootPackageWorkspaces(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"packages/*"}) {
		t.Fatalf("got %v", got)
	}
}

func TestParseRootPackageWorkspacesObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	writeFile(t, path, `{"workspaces": {"packages": ["apps/*"]}}`)
	got, err := parseRootPackageWorkspaces(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"apps/*"}) {
		t.Fatalf("got %v", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
