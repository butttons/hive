package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

type cfBucket struct {
	Name string `json:"name"`
}

func listBuckets(ctx context.Context, accountID string) ([]cfBucket, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/accounts/"+accountID+"/r2/buckets", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Buckets []cfBucket `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse buckets: %w", err)
	}
	return res.Buckets, nil
}

func createBucket(ctx context.Context, accountID, name, jurisdiction string) error {
	body := map[string]string{"name": name}
	if jurisdiction != "" {
		body["jurisdiction"] = jurisdiction
	}
	_, err := cfRequest(ctx, http.MethodPost, "/accounts/"+accountID+"/r2/buckets", nil, body, nil)
	return err
}

// r2Endpoint returns the S3 endpoint for an account, honoring jurisdiction.
func r2Endpoint(accountID, jurisdiction string) string {
	if jurisdiction == "" || jurisdiction == "default" {
		return fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
	}
	return fmt.Sprintf("https://%s.%s.r2.cloudflarestorage.com", accountID, jurisdiction)
}

// discoverAccount returns the single account visible through the token's
// zones, or an error when zero or several accounts are visible.
func discoverAccount(ctx context.Context) (string, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/zones", url.Values{"per_page": {"50"}}, nil, nil)
	if err != nil {
		return "", err
	}
	var zones []cfZone
	if err := json.Unmarshal(raw, &zones); err != nil {
		return "", fmt.Errorf("parse zones: %w", err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, z := range zones {
		if !seen[z.Account.ID] {
			seen[z.Account.ID] = true
			ids = append(ids, z.Account.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no accounts visible through zones; pass the account explicitly (not supported yet)")
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("multiple accounts visible (%s); cannot pick one", strings.Join(ids, ", "))
	}
}

type cfPermissionGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// mintR2Token creates a user API token scoped to object read/write on one
// bucket and derives the S3 key pair from it: the Access Key ID is the
// token's id and the Secret Access Key is the SHA-256 of the token value.
func mintR2Token(ctx context.Context, accountID, bucket, jurisdiction, name string) (string, string, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/user/tokens/permission_groups", nil, nil, nil)
	if err != nil {
		return "", "", err
	}
	var groups []cfPermissionGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return "", "", fmt.Errorf("parse permission groups: %w", err)
	}
	var pgID string
	for _, g := range groups {
		if g.Name == "Workers R2 Storage Bucket Item Write" {
			pgID = g.ID
			break
		}
	}
	if pgID == "" {
		return "", "", fmt.Errorf("permission group \"Workers R2 Storage Bucket Item Write\" not found")
	}
	if jurisdiction == "" {
		jurisdiction = "default"
	}
	resource := fmt.Sprintf("com.cloudflare.edge.r2.bucket.%s_%s_%s", accountID, jurisdiction, bucket)
	body := map[string]any{
		"name": name,
		"policies": []map[string]any{{
			"effect":            "allow",
			"resources":         map[string]string{resource: "*"},
			"permission_groups": []map[string]string{{"id": pgID}},
		}},
	}
	raw, err = cfRequest(ctx, http.MethodPost, "/user/tokens", nil, body, nil)
	if err != nil {
		return "", "", err
	}
	var res struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", fmt.Errorf("parse created token: %w", err)
	}
	sum := sha256.Sum256([]byte(res.Value))
	return res.ID, hex.EncodeToString(sum[:]), nil
}

// writeAppEnvFile persists celld-relevant variables to
// ~/.config/hive/<app>.env with 0600 permissions.
func writeAppEnvFile(app *App, kv map[string]string, force bool) error {
	path := appEnvFilePath(app)
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s exists (use --force to overwrite)", path)
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, shellSingleQuote(kv[k]))
	}
	if err := os.MkdirAll(appEnvDir(), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func appEnvDir() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/hive"
}

func cmdInit(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	bucketFlag := fs.String("bucket", "", "R2 bucket name (created if missing)")
	jurisdiction := fs.String("jurisdiction", "", "bucket jurisdiction (e.g. eu)")
	accessKey := fs.String("access-key", "", "existing R2 Access Key ID")
	secretKey := fs.String("secret-key", "", "existing R2 Secret Access Key")
	force := fs.Bool("force", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if (*accessKey == "") != (*secretKey == "") {
		return fmt.Errorf("--access-key and --secret-key must be given together")
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	accountID, err := discoverAccount(ctx)
	if err != nil {
		return err
	}

	buckets, err := listBuckets(ctx, accountID)
	if err != nil {
		return err
	}
	bucket := *bucketFlag
	createdBucket := false
	if bucket == "" {
		switch len(buckets) {
		case 0:
			return fmt.Errorf("no buckets in this account; pass --bucket to create one")
		case 1:
			bucket = buckets[0].Name
		default:
			names := make([]string, 0, len(buckets))
			for _, b := range buckets {
				names = append(names, b.Name)
			}
			return fmt.Errorf("multiple buckets (%s); pass --bucket", strings.Join(names, ", "))
		}
	} else {
		found := false
		for _, b := range buckets {
			if b.Name == bucket {
				found = true
			}
		}
		if !found {
			if err := createBucket(ctx, accountID, bucket, *jurisdiction); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
			createdBucket = true
		}
	}

	var keyID, keySecret, credSource string
	if *accessKey != "" {
		keyID, keySecret, credSource = *accessKey, *secretKey, "pasted"
	} else {
		keyID, keySecret, err = mintR2Token(ctx, accountID, bucket, *jurisdiction, "hive-"+app.Name+"-"+bucket)
		if err != nil {
			link := fmt.Sprintf("https://dash.cloudflare.com/%s/r2/api-tokens", accountID)
			return fmt.Errorf("mint R2 API token: %w\nfallback: create one at %s (Object Read & Write on %s), then rerun with --access-key/--secret-key", err, link, bucket)
		}
		credSource = "minted"
	}

	env := map[string]string{
		"CELLD_BUCKET":          bucket,
		"S3_ENDPOINT":           r2Endpoint(accountID, *jurisdiction),
		"AWS_REGION":            "auto",
		"AWS_ACCESS_KEY_ID":     keyID,
		"AWS_SECRET_ACCESS_KEY": keySecret,
	}
	if err := writeAppEnvFile(app, env, *force); err != nil {
		return err
	}

	if *jsonFlag {
		res := struct {
			App           string `json:"app"`
			Bucket        string `json:"bucket"`
			BucketCreated bool   `json:"bucket_created"`
			Endpoint      string `json:"endpoint"`
			CredSource    string `json:"cred_source"`
			EnvFile       string `json:"env_file"`
		}{app.Name, bucket, createdBucket, env["S3_ENDPOINT"], credSource, appEnvFilePath(app)}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	if createdBucket {
		fmt.Printf("created bucket %q\n", bucket)
	}
	fmt.Printf("bucket: %s (endpoint %s)\n", bucket, env["S3_ENDPOINT"])
	fmt.Printf("credentials: %s, scoped to bucket %q\n", credSource, bucket)
	fmt.Printf("wrote %s (0600)\n", appEnvFilePath(app))
	return nil
}
