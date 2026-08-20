package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

type cfEnvelope struct {
	Success bool             `json:"success"`
	Errors  []cfError        `json:"errors"`
	Result  json.RawMessage  `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func cfRequest(ctx context.Context, method, path string, query url.Values, body any, out json.RawMessage) (json.RawMessage, error) {
	token, err := cfAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	u := cfAPIBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var env cfEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("parse response from %s %s: %w", method, path, err)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d: %s", e.Code, e.Message))
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, strings.Join(msgs, "; "))
	}
	return env.Result, nil
}

type cfZone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

// findZone resolves a hostname to its Cloudflare zone by longest suffix
// match over the zones visible to the token. The zone record also carries
// the account ID, which is how tunnel API calls get their account path.
func findZone(ctx context.Context, hostname string) (*cfZone, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/zones", url.Values{"per_page": {"50"}}, nil, nil)
	if err != nil {
		return nil, err
	}
	var zones []cfZone
	if err := json.Unmarshal(raw, &zones); err != nil {
		return nil, fmt.Errorf("parse zones: %w", err)
	}
	var best *cfZone
	for i := range zones {
		z := &zones[i]
		if hostname == z.Name || strings.HasSuffix(hostname, "."+z.Name) {
			if best == nil || len(z.Name) > len(best.Name) {
				best = z
			}
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no zone covers %q (is it in this Cloudflare account?)", hostname)
	}
	return best, nil
}

type cfTunnel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Token  string `json:"token"`
	Status string `json:"status"`
}

func findTunnel(ctx context.Context, accountID, name string) (*cfTunnel, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel",
		url.Values{"name": {name}, "is_deleted": {"false"}}, nil, nil)
	if err != nil {
		return nil, err
	}
	var tunnels []cfTunnel
	if err := json.Unmarshal(raw, &tunnels); err != nil {
		return nil, fmt.Errorf("parse tunnels: %w", err)
	}
	if len(tunnels) == 0 {
		return nil, nil
	}
	return &tunnels[0], nil
}

func createTunnel(ctx context.Context, accountID, name string) (*cfTunnel, error) {
	raw, err := cfRequest(ctx, http.MethodPost, "/accounts/"+accountID+"/cfd_tunnel", nil,
		map[string]string{"name": name, "config_src": "cloudflare"}, nil)
	if err != nil {
		return nil, err
	}
	var t cfTunnel
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse created tunnel: %w", err)
	}
	return &t, nil
}

func tunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/token", nil, nil, nil)
	if err != nil {
		return "", err
	}
	var tok string
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", fmt.Errorf("parse tunnel token: %w", err)
	}
	return tok, nil
}

type ingressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

func getTunnelConfig(ctx context.Context, accountID, tunnelID string) ([]ingressRule, error) {
	raw, err := cfRequest(ctx, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Config struct {
			Ingress []ingressRule `json:"ingress"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse tunnel config: %w", err)
	}
	return cfg.Config.Ingress, nil
}

// syncIngress ensures rules for hostnames exist in the tunnel config,
// preserving other rules and keeping the catchall last. Returns true if
// the config was changed.
func syncIngress(ctx context.Context, accountID, tunnelID string, want []ingressRule) (bool, error) {
	current, err := getTunnelConfig(ctx, accountID, tunnelID)
	if err != nil {
		return false, err
	}
	var catchall *ingressRule
	rules := make([]ingressRule, 0, len(current)+len(want))
	for _, r := range current {
		if r.Hostname == "" {
			c := r
			catchall = &c
			continue
		}
		rules = append(rules, r)
	}
	if catchall == nil {
		catchall = &ingressRule{Service: "http_status:404"}
	}
	changed := false
	for _, w := range want {
		found := false
		for i, r := range rules {
			if r.Hostname == w.Hostname {
				found = true
				if r.Service != w.Service {
					rules[i].Service = w.Service
					changed = true
				}
			}
		}
		if !found {
			rules = append(rules, w)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	body := map[string]any{"config": map[string]any{"ingress": append(rules, *catchall)}}
	if _, err := cfRequest(ctx, http.MethodPut, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations", nil, body, nil); err != nil {
		return false, err
	}
	return true, nil
}

type cfDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// upsertTunnelCNAME points hostname at the tunnel (CNAME to
// <tunnel-id>.cfargotunnel.com, proxied), updating any stale record.
func upsertTunnelCNAME(ctx context.Context, zoneID, hostname, tunnelID string) (bool, error) {
	target := tunnelID + ".cfargotunnel.com"
	raw, err := cfRequest(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records",
		url.Values{"name": {hostname}}, nil, nil)
	if err != nil {
		return false, err
	}
	var records []cfDNSRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return false, fmt.Errorf("parse dns records: %w", err)
	}
	for _, r := range records {
		if (r.Type == "CNAME" || r.Type == "A" || r.Type == "AAAA") && r.Content != target {
			if _, err := cfRequest(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+r.ID, nil, nil, nil); err != nil {
				return false, fmt.Errorf("delete stale %s record for %s: %w", r.Type, hostname, err)
			}
		} else if r.Type == "CNAME" && r.Content == target {
			return false, nil
		}
	}
	body := map[string]any{"type": "CNAME", "name": hostname, "content": target, "proxied": true}
	if _, err := cfRequest(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", nil, body, nil); err != nil {
		return false, fmt.Errorf("create CNAME for %s: %w", hostname, err)
	}
	return true, nil
}

func cmdTunnel(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{})
	fs := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "")
	name := fs.String("name", "hive", "tunnel name")
	withSSH := fs.Bool("ssh", false, "also route ssh.<zone> for CI access")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}
	if app.Hive.Domain == "" {
		return fmt.Errorf("no domain in the hive block of %s/package.json", app.Dir)
	}

	zone, err := findZone(ctx, app.Hive.Domain)
	if err != nil {
		return err
	}
	tunnel, err := findTunnel(ctx, zone.Account.ID, *name)
	if err != nil {
		return err
	}
	created := false
	if tunnel == nil {
		tunnel, err = createTunnel(ctx, zone.Account.ID, *name)
		if err != nil {
			return err
		}
		created = true
	}

	want := []ingressRule{{Hostname: app.Hive.Domain, Service: fmt.Sprintf("http://localhost:%d", app.Hive.Port)}}
	if *withSSH {
		want = append(want, ingressRule{Hostname: "ssh." + zone.Name, Service: "ssh://localhost:22"})
	}
	ingressChanged, err := syncIngress(ctx, zone.Account.ID, tunnel.ID, want)
	if err != nil {
		return err
	}

	dnsChanged, err := upsertTunnelCNAME(ctx, zone.ID, app.Hive.Domain, tunnel.ID)
	if err != nil {
		return err
	}
	if *withSSH {
		if _, err := upsertTunnelCNAME(ctx, zone.ID, "ssh."+zone.Name, tunnel.ID); err != nil {
			return err
		}
	}

	tok, err := tunnelToken(ctx, zone.Account.ID, tunnel.ID)
	if err != nil {
		return err
	}

	if *jsonFlag {
		res := struct {
			TunnelID       string `json:"tunnel_id"`
			TunnelName     string `json:"tunnel_name"`
			Created        bool   `json:"created"`
			IngressChanged bool   `json:"ingress_changed"`
			DNSChanged     bool   `json:"dns_changed"`
			Domain         string `json:"domain"`
			Token          string `json:"token"`
		}{tunnel.ID, *name, created, ingressChanged, dnsChanged, app.Hive.Domain, tok}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}

	if created {
		fmt.Printf("created tunnel %q (%s)\n", *name, tunnel.ID)
	} else {
		fmt.Printf("tunnel %q (%s)\n", *name, tunnel.ID)
	}
	if ingressChanged {
		fmt.Printf("ingress: %s -> localhost:%d\n", app.Hive.Domain, app.Hive.Port)
	} else {
		fmt.Printf("ingress: %s already routed to localhost:%d\n", app.Hive.Domain, app.Hive.Port)
	}
	if dnsChanged {
		fmt.Printf("dns: %s -> tunnel\n", app.Hive.Domain)
	} else {
		fmt.Printf("dns: %s already points at the tunnel\n", app.Hive.Domain)
	}
	fmt.Println("install on the box with: cloudflared service install <token>")
	fmt.Println("token: run `hive tunnel --json` (keep it secret)")
	return nil
}
