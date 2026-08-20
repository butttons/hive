# hive

A wrangler-shaped CLI for [celld](https://celld.dev/docs) — Deno's self-hosted, distributed Durable Objects daemon. Bring your existing Worker; hive tells you if it runs on celld, then deploys and runs it on your own hardware.

Go, stdlib only. Single static binary, zero dependencies.

## What you need

- An S3-compatible bucket with conditional writes + read-after-write consistency (Cloudflare R2, AWS S3, Tigris qualify; B2/MinIO/Spaces do not), and its credentials.
- A box to run nodes on: Linux (amd64/arm64) or macOS (arm64). Docker on the box if you want the supervised backend.
- [celld](https://celld.dev/docs) installed wherever you deploy *from* (`curl -fsSL https://celld.dev/install.sh | sh`), plus `esbuild` on PATH for deploys.

Cloudflare is optional: `login`/`init`/`tunnel` automate R2 + Tunnel + DNS, but any S3 credentials and any reverse proxy (caddy, nginx, your own tunnel) work just as well.

## Build

```sh
CGO_ENABLED=0 go build -o hive .
```

Cross-compiles to `GOOS/GOARCH` = linux/amd64, linux/arm64, darwin/arm64 — same targets as celld.

## Quickstart

```sh
hive add myapp            # scaffold app, allocates a free port
cd myapp

# credentials: either export AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY /
# S3_ENDPOINT / AWS_REGION / CELLD_BUCKET yourself, or provision with:
hive login                # Cloudflare OAuth consent (optional)
hive init --bucket mybucket

hive check                # will this deploy to celld?
hive deploy               # tsc → celld deploy → restart node → health gate
curl http://127.0.0.1:<port>/
```

## Config model

Two files per app, same directory:

- `wrangler.jsonc` — pristine, celld-legal keys only (`name`, `main`, `compatibility_date`, `durable_objects`, `migrations`, …). This is the deploy contract handed to celld untouched.
- `package.json` — a `"hive"` block for everything celld rejects:

```json
{
  "hive": { "port": 8101, "domain": "app.example.com", "server": "user@box", "backend": "docker" }
}
```

`port` is required. `domain`, `server`, `backend` are optional.

## Commands

| command | what it does |
| --- | --- |
| `hive add <name>` | Scaffold a new app (wrangler.jsonc + index.ts + tsconfig + package.json), allocate a free port |
| `hive check` | Validate wrangler.jsonc against the celld-legal key list, dry-run the deploy path |
| `hive deploy` | Typecheck → `celld deploy` → restart the node → wait for `/__celld/health` |
| `hive up` / `hive down` | Start/stop the node. Idempotent; drift → restart |
| `hive status` | App config + node state. `--json` is the primary interface |
| `hive login` | Cloudflare OAuth (PKCE). `--export` prints `export CLOUDFLARE_API_TOKEN=…` instead of saving |
| `hive init` | Provision an R2 bucket + S3 key pair into `~/.config/hive/<app>.env` (0600) |
| `hive tunnel` | Create/sync a remotely-managed Cloudflare Tunnel: ingress rules + DNS, prints the box install command |

Every command takes `--json`. Exit codes are meaningful; errors come from celld/Cloudflare unchanged.

## Run backends

- **process** (default): `celld` as a detached process, logs to `.hive/node.log`, `down` = SIGTERM (celld drains gracefully). Zero moving parts; no supervisor.
- **docker** (`--docker` or `"backend": "docker"`): node runs as container `hive-<app>` from image `hive/celld:<version>`, published on `127.0.0.1:<port>`, `--restart unless-stopped`. `docker stop` is the same graceful drain.

## Remote operation

Set `"server": "user@box"` in the hive block and commands proxy to the hive binary on the box over SSH (`ssh box hive <cmd> --local`). No RPC protocol; the same backends run there. `celld deploy` itself is bucket-direct from anywhere — only restart and health checks touch the box.

Bare box setup: docker, the hive binary, nothing else. Credentials sync over SSH as `~/.config/hive/<app>.env` (0600).

CI: a stock GitHub-hosted runner runs `hive deploy`; it reaches the box via SSH through the Cloudflare Tunnel (`ssh.<domain>` ingress rule, `sshd` on loopback, cloudflared as ProxyCommand). Secrets: bucket creds, SSH key, tunnel token.

## Security notes

- Bucket credentials = full fleet control (deployments, cell replicas, ownership records). Keep them in env vars or `~/.config/hive/*.env` — never in the repo.
- celld's internal/operator listener is unauthenticated. It never leaves loopback; the public listener binds `127.0.0.1` behind your proxy/tunnel.
- `CLOUDFLARE_API_TOKEN` in the environment wins over the stored OAuth token; `~/.config/hive/auth.json` is 0600 and refreshable.
