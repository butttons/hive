package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.log")

	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got := tailFile(path, 20)
	lines := strings.Split(got, "\n")
	if len(lines) != 20 {
		t.Fatalf("expected 20 lines, got %d", len(lines))
	}
	if lines[0] != "line 11" || lines[19] != "line 30" {
		t.Fatalf("expected lines 11..30, got %q..%q", lines[0], lines[19])
	}
}

func TestTailFileShortFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.log")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailFile(path, 20); got != "one\ntwo" {
		t.Fatalf("expected whole file, got %q", got)
	}
}

func TestTailFileMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := tailFile(filepath.Join(dir, "nope.log"), 20); !strings.HasPrefix(got, "(no log at") {
		t.Fatalf("expected missing-file placeholder, got %q", got)
	}
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailFile(empty, 20); !strings.HasPrefix(got, "(log") {
		t.Fatalf("expected empty-file placeholder, got %q", got)
	}
}
