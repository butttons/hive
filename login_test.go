package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveTokenCreatesFileWithRestrictedPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tr := &tokenResponse{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresIn:    3600,
		Scope:        loginScope,
	}
	if err := saveToken(tr); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	path := filepath.Join(tmpDir, ".config", "hive", "auth.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("auth.json mode = %o, want 0600", info.Mode().Perm())
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if !strings.Contains(string(b), "test-access-token") {
		t.Fatalf("auth.json missing access_token")
	}
	if !strings.Contains(string(b), "test-refresh-token") {
		t.Fatalf("auth.json missing refresh_token")
	}
	if !strings.Contains(string(b), loginScope) {
		t.Fatalf("auth.json missing scope")
	}
}

func TestLoadTokenMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, err := loadToken(context.Background())
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "run hive login") {
		t.Fatalf("error should point at hive login: %v", err)
	}
}

func TestBuildAuthURLWellFormed(t *testing.T) {
	u := buildAuthURL("client-id", redirectURI(), loginScope, "state-value", "challenge-value")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := parsed.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "client-id",
		"redirect_uri":          redirectURI(),
		"scope":                 loginScope,
		"state":                 "state-value",
		"code_challenge":        "challenge-value",
		"code_challenge_method": "S256",
	}
	for key, want := range checks {
		if got := q.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatal("verifier or challenge empty")
	}
	if len(verifier) < 32 {
		t.Fatalf("verifier too short: %d", len(verifier))
	}
}

func TestExchangeBogusCode(t *testing.T) {
	if os.Getenv("HIVE_TEST_NETWORK") != "1" {
		t.Skip("set HIVE_TEST_NETWORK=1 to hit the real Cloudflare token endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := exchangeCode(ctx, "bogus-code", "verifier", redirectURI())
	if err == nil {
		t.Fatal("expected error for bogus code")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 from token endpoint, got: %v", err)
	}
}
