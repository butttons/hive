package main

import (
	"path/filepath"
	"testing"
)

func TestLoadAppRejectsRelativeDir(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "package.json"), `{"name":"x","hive":{"port":8101,"dir":"relative/path"}}`)
	writeFile(t, filepath.Join(tmp, "wrangler.jsonc"), `{"name":"x","main":"index.ts"}`)

	_, err := LoadApp(tmp)
	if err == nil {
		t.Fatal("expected error for relative hive.dir")
	}
	if !contains(err.Error(), "hive.dir must be an absolute path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadAppAcceptsAbsoluteDir(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "package.json"), `{"name":"x","hive":{"port":8101,"dir":"/opt/apps/x"}}`)
	writeFile(t, filepath.Join(tmp, "wrangler.jsonc"), `{"name":"x","main":"index.ts"}`)

	app, err := LoadApp(tmp)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if app.Hive.Dir != "/opt/apps/x" {
		t.Fatalf("hive.dir = %q, want /opt/apps/x", app.Hive.Dir)
	}
}

func TestRemoteAppDirPrefersHiveDir(t *testing.T) {
	app := &App{Dir: "/Users/x/a", Hive: HiveConfig{Dir: "/home/x/a", Port: 8101}}
	got, err := remoteAppDir(app)
	if err != nil {
		t.Fatalf("remoteAppDir: %v", err)
	}
	if got != "/home/x/a" {
		t.Fatalf("remoteAppDir = %q, want /home/x/a", got)
	}
}

func TestRemoteAppDirFallsBackToLocalDir(t *testing.T) {
	app := &App{Dir: "/Users/x/a", Hive: HiveConfig{Port: 8101}}
	got, err := remoteAppDir(app)
	if err != nil {
		t.Fatalf("remoteAppDir: %v", err)
	}
	if got != "/Users/x/a" {
		t.Fatalf("remoteAppDir = %q, want /Users/x/a", got)
	}
}

func TestRemoteAppDirRequiresAbsolute(t *testing.T) {
	app := &App{Dir: "/Users/x/a", Hive: HiveConfig{Dir: "relative", Port: 8101}}
	_, err := remoteAppDir(app)
	if err == nil {
		t.Fatal("expected error for relative hive.dir")
	}
	if !contains(err.Error(), "hive.dir must be an absolute path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncAppFilesNoOpWhenDirEmpty(t *testing.T) {
	app := &App{Dir: t.TempDir(), Name: "x", Hive: HiveConfig{Port: 8101}}
	writeFile(t, filepath.Join(app.Dir, "package.json"), `{"name":"x","hive":{"port":8101}}`)
	writeFile(t, filepath.Join(app.Dir, "wrangler.jsonc"), `{"name":"x","main":"index.ts"}`)

	if err := syncAppFiles("no-such-server", app); err != nil {
		t.Fatalf("syncAppFiles should be a no-op when dir is empty: %v", err)
	}
}

func TestSyncAppFilesErrorsOnMissingLocalFiles(t *testing.T) {
	tmp := t.TempDir()
	app := &App{Dir: tmp, Name: "x", Hive: HiveConfig{Port: 8101, Dir: "/opt/apps/x"}}

	if err := syncAppFiles("no-such-server", app); err == nil {
		t.Fatal("expected error when local files missing")
	}
}
