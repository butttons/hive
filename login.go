package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultClientID     = "d6188eb87e7198f8f9fd8ef81abc6539"
	defaultAuthURL      = "https://dash.cloudflare.com/oauth2/auth"
	defaultTokenURL     = "https://dash.cloudflare.com/oauth2/token"
	defaultRedirectHost = "127.0.0.1"
	defaultRedirectPath = "/callback"
	defaultLoginPort    = "8976"
	loginScope          = "argotunnel.write dns.write zone.read workers-r2.write"
)

func clientID() string {
	if v := os.Getenv("HIVE_CF_CLIENT_ID"); v != "" {
		return v
	}
	return defaultClientID
}

func loginPort() string {
	if v := os.Getenv("HIVE_LOGIN_PORT"); v != "" {
		return v
	}
	return defaultLoginPort
}

func loginTimeout() time.Duration {
	if v := os.Getenv("HIVE_LOGIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 5 * time.Minute
}

func redirectURI() string {
	return fmt.Sprintf("http://%s:%s%s", defaultRedirectHost, loginPort(), defaultRedirectPath)
}

func cmdLogin(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	statusFlag := fs.Bool("status", false, "")
	noBrowser := fs.Bool("no-browser", false, "")
	exportFlag := fs.Bool("export", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *statusFlag {
		return loginStatus(ctx, *jsonFlag)
	}
	return runLogin(ctx, *jsonFlag, *noBrowser, *exportFlag)
}

func loginStatus(ctx context.Context, jsonMode bool) error {
	if v := os.Getenv("CLOUDFLARE_API_TOKEN"); v != "" {
		if jsonMode {
			fmt.Println(`{"logged_in": true, "source": "env"}`)
		} else {
			fmt.Println("Using CLOUDFLARE_API_TOKEN from the environment.")
		}
		return nil
	}
	tok, err := loadToken(ctx)
	if jsonMode {
		st := struct {
			LoggedIn  bool   `json:"logged_in"`
			ExpiresAt string `json:"expires_at,omitempty"`
			Scope     string `json:"scope,omitempty"`
			Error     string `json:"error,omitempty"`
		}{}
		if err != nil {
			st.Error = err.Error()
		} else {
			st.LoggedIn = true
			st.ExpiresAt = tok.ExpiresAt.Format(time.RFC3339)
			st.Scope = tok.Scope
		}
		b, merr := json.MarshalIndent(st, "", "  ")
		if merr != nil {
			return fmt.Errorf("marshal status: %w", merr)
		}
		fmt.Println(string(b))
		return nil
	}
	if err != nil {
		fmt.Printf("Not logged in: %s\n", err)
		return nil
	}
	fmt.Printf("Logged in to Cloudflare\n")
	fmt.Printf("Expires: %s\n", tok.ExpiresAt.Format(time.RFC3339))
	fmt.Printf("Scopes: %s\n", tok.Scope)
	return nil
}

func runLogin(ctx context.Context, jsonMode, noBrowser, exportMode bool) error {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return fmt.Errorf("generate state: %w", err)
	}

	authURL := buildAuthURL(clientID(), redirectURI(), loginScope, state, challenge)
	fmt.Println("Open this URL in your browser to authorize hive:")
	fmt.Println(authURL)
	if !noBrowser {
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(os.Stderr, "could not open browser: %v\n", err)
		}
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, loginTimeout())
	defer cancel()

	code, err := receiveCallback(exchangeCtx, state)
	if err != nil {
		return fmt.Errorf("callback: %w", err)
	}

	tr, err := exchangeCode(exchangeCtx, code, verifier, redirectURI())
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	if exportMode {
		fmt.Printf("export CLOUDFLARE_API_TOKEN=%s\n", shellSingleQuote(tr.AccessToken))
		return nil
	}
	if err := saveToken(tr); err != nil {
		return fmt.Errorf("save token: %w", err)
	}

	if jsonMode {
		res := struct {
			OK    bool   `json:"ok"`
			Scope string `json:"scope"`
		}{OK: true, Scope: effectiveScope(tr)}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("Authorized. Scopes: %s\n", effectiveScope(tr))
	fmt.Println("Token saved. To use env-var auth instead: hive login --export")
	return nil
}

func effectiveScope(tr *tokenResponse) string {
	if tr.Scope != "" {
		return tr.Scope
	}
	return loginScope
}

func generatePKCE() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("read random: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func buildAuthURL(clientID, redirectURI, scope, state, challenge string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("scope", scope)
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	return defaultAuthURL + "?" + v.Encode()
}

func openBrowser(target string) error {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
	c := exec.Command(cmd, target)
	return c.Start()
}

func receiveCallback(ctx context.Context, state string) (string, error) {
	mux := http.NewServeMux()
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux.HandleFunc(defaultRedirectPath, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			errCh <- fmt.Errorf("parse callback: %w", err)
			return
		}
		if errStr := r.FormValue("error"); errStr != "" {
			http.Error(w, fmt.Sprintf("authorization error: %s", errStr), http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization error: %s (%s)", errStr, r.FormValue("error_description"))
			return
		}
		if r.FormValue("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("callback state mismatch")
			return
		}
		code := r.FormValue("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("callback missing authorization code")
			return
		}
		fmt.Fprintf(w, "hive: authorized. You can close this tab.")
		codeCh <- code
	})

	srv := &http.Server{
		Addr:    defaultRedirectHost + ":" + loginPort(),
		Handler: mux,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("callback server: %w", err)
		}
	}()

	select {
	case code := <-codeCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return code, nil
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return "", err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return "", fmt.Errorf("timed out waiting for browser callback: %w", ctx.Err())
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
}

func exchangeCode(ctx context.Context, code, verifier, redirectURI string) (*tokenResponse, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", clientID())
	data.Set("code", code)
	data.Set("code_verifier", verifier)
	data.Set("redirect_uri", redirectURI)
	return postToken(ctx, data)
}

func refreshAccessToken(ctx context.Context, refreshToken string) (*tokenResponse, error) {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", clientID())
	data.Set("refresh_token", refreshToken)
	return postToken(ctx, data)
}

func postToken(ctx context.Context, data url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, defaultTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	return &tr, nil
}

func tokenFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hive", "auth.json")
}

// cfAccessToken resolves the Cloudflare credential. CLOUDFLARE_API_TOKEN in
// the environment is the source of truth; the OAuth token file written by
// hive login is the fallback for users who went through the consent flow.
func cfAccessToken(ctx context.Context) (string, error) {
	if v := os.Getenv("CLOUDFLARE_API_TOKEN"); v != "" {
		return v, nil
	}
	tok, err := loadToken(ctx)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func ensureConfigDir() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home: %w", err)
	}
	dir := filepath.Join(home, ".config", "hive")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return nil
}

func saveToken(tr *tokenResponse) error {
	tok := token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scope:        effectiveScope(tr),
	}
	if err := ensureConfigDir(); err != nil {
		return err
	}
	path := tokenFilePath()
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func loadToken(ctx context.Context) (*token, error) {
	path := tokenFilePath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not logged in to Cloudflare: run hive login")
		}
		return nil, fmt.Errorf("read token file: %w", err)
	}
	var tok token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	if time.Now().Before(tok.ExpiresAt.Add(-30 * time.Second)) {
		return &tok, nil
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("token expired and no refresh token available: run hive login")
	}
	tr, err := refreshAccessToken(ctx, tok.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("refresh token failed: %w; run hive login", err)
	}
	tok = token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scope:        effectiveScope(tr),
	}
	if err := saveToken(tr); err != nil {
		return nil, fmt.Errorf("save refreshed token: %w", err)
	}
	return &tok, nil
}
