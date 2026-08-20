package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// s3Get fetches a key from the fleet bucket using SigV4, stdlib only.
// Credentials come from the process environment (already fed by
// ~/.config/hive/<app>.env via loadAppEnv).
func s3Get(ctx context.Context, key string) ([]byte, error) {
	endpoint := strings.TrimSuffix(os.Getenv("S3_ENDPOINT"), "/")
	region := os.Getenv("AWS_REGION")
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("S3_ENDPOINT, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set")
	}
	if region == "" {
		region = "auto"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint+"/"+s3EscapePath(key), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex([]byte{})

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	if tok := os.Getenv("AWS_SESSION_TOKEN"); tok != "" {
		req.Header.Set("X-Amz-Security-Token", tok)
		signedHeaders += ";x-amz-security-token"
	}

	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	if tok := os.Getenv("AWS_SESSION_TOKEN"); tok != "" {
		canonicalHeaders += "x-amz-security-token:" + tok + "\n"
	}
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		req.URL.EscapedPath(),
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" +
		sha256Hex([]byte(canonicalRequest))

	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", key, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// parseCurrentVersion extracts the version from a bucket deploy manifest
// (deploy/current.json), returning "unknown" for anything unexpected.
func parseCurrentVersion(b []byte) string {
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil || manifest.Version == "" {
		return "unknown"
	}
	return manifest.Version
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// s3EscapePath escapes each path segment per RFC 3986, keeping slashes.
func s3EscapePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(s3Escape(p), "+", "%20")
	}
	return strings.Join(parts, "/")
}

func s3Escape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
