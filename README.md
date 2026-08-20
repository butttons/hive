# hive

A wrangler-shaped CLI for [celld](https://celld.dev/docs) — Deno's self-hosted, distributed Durable Objects daemon. Bring your existing Worker; hive tells you if it runs on celld, then deploys and runs it on your own hardware.

Go, stdlib only. Single static binary, zero dependencies, instant cold start.

## What you need

- An S3-compatible bucket with conditional writes + read-after-write consistency (Cloudflare R2, AWS S3, Tigris qualify; B2/MinIO/Spaces do not), and its credentials.
- A box to run nodes on: Linux (amd64/arm64) or macOS (arm64). Docker on the box if you want the supervised backend.
- [celld](https://celld.dev/docs) installed wherever you deploy *from* (`curl -fsSL https://celld.dev/install.sh | sh`), plus `esbuild` on PATH for deploys.

Cloudflare is optional: `cf login`/`init`/`cf tunnel` automate R2 + Tunnel + DNS, but any S3 credentials and any reverse proxy (caddy, nginx, your own tunnel) work just as well.

## Install

```sh
curl -fsSL https://hive.butttons.dev/setup.sh | bash
```

Installs the latest release binary to `~/.local/bin/hive` (checksum-verified; `HIVE_INSTALL_DIR` overrides). Or build from source:

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
hive init --bucket mybucket --access-key ... --secret-key ... --endpoint ...

hive deploy               # tsc → celld deploy → restart node → health gate
curl http://127.0.0.1:<port>/
```

There is no watch mode or HMR — the restart is the reload. The loop is `edit → hive deploy → curl`.

## Config model

Two files per app, same directory:

- `wrangler.jsonc` — pristine, celld-legal keys only (`name`, `main`, `compatibility_date`, `durable_objects`, `migrations`, `vars`, `assets`, `services`, …). This is the deploy contract handed to celld untouched. celld hard-fails on anything else.
- `package.json` — a `"hive"` block for everything celld rejects:

```json
{
  "hive": { "port": 8101, "domain": "app.example.com", "server": "user@box", "dir": "/opt/apps/myapp", "backend": "docker" }
}
```

`port` is required. `domain`, `server`, `dir`, `backend` are optional (local-only apps skip them). When `server` is set, `dir` is the absolute path of the app directory on the server; if omitted, the local path is used verbatim, which only works when the box shares the same filesystem layout.

## Monorepos

Run `hive` from a workspace root to operate on the whole fleet. Discovery order:

1. `pnpm-workspace.yml` / `pnpm-workspace.yaml` `packages:` globs.
2. Root `package.json` `workspaces` field (array or `{packages:[...]}` object).

A directory counts as a hive app only if its `package.json` contains a `"hive"` block. Patterns support a single trailing `/*` (e.g. `apps/*`); `**` is not required.

| command | from workspace root | from an app dir |
| --- | --- | --- |
| `hive status` | compact fleet table | single-app status |
| `hive ui` | fleet dashboard | single-app dashboard |
| `hive deploy all` | deploy every app sequentially | deploy current app |
| `hive deploy --filter <name>` | deploy one matching app | same |
| `hive status --filter <name>` | single-app status | same |

`--filter` matches the app's `package.json` `name` or the directory basename; it errors listing available apps if nothing matches. If no workspace file exists, `--packages <glob>` (repeatable or comma-separated) enumerates apps manually.

## Commands

Every command takes `--json` and prints machine-readable output. Agents are the primary user.

| command | flags | what it does |
| --- | --- | --- |
| `hive add <name>` | `--port`, `--force` | Scaffold a new app (wrangler.jsonc + index.ts + tsconfig + package.json), allocate a free port |
| `hive deploy` | `--docker`, `--local`, `--no-restart` | Typecheck (`tsc -b`) → `celld deploy` → restart the node → wait for `/__celld/health` (30s gate). Prints the version ID. `--no-restart` uploads only, for externally supervised nodes (Coolify & co.) |
| `hive deploy all` | `--docker`, `--local`, `--packages` | Deploy every app in the workspace sequentially, continuing past failures |
| `hive up` | `--docker`, `--local` | Start the node. Idempotent; config drift → restart |
| `hive down` | `--local` | Stop the node gracefully (SIGTERM; celld drains in-flight work) |
| `hive status` | `--local`, `--filter` | App config + node state; from a workspace root, show a compact fleet table |
| `hive init` | `--bucket`, `--access-key`, `--secret-key`, `--endpoint`, `--region`, `--jurisdiction`, `--force` | Provision bucket credentials into `~/.config/hive/<app>.env` (0600) |
| `hive bootstrap` | — | Install hive + celld at `~/.local/bin` on the app's server (idempotent) |
| `hive cf login` | `--status`, `--export`, `--no-browser` | Cloudflare OAuth consent (PKCE); stores a refreshable token |
| `hive cf tunnel` | `--name`, `--ssh` | Create/sync a remotely-managed Cloudflare Tunnel: ingress rules + DNS, prints the box install command |
| `hive exe new <name>` | — | Create an exe.dev VM and wait for its DNS to propagate |
| `hive exe share` | `--private` | Point the exe.dev HTTPS proxy at the app's port; public by default |
| `hive exe domain` | — | CNAME the app's domain to the VM (DNS-only, via Cloudflare creds) and register it with exe.dev |

### `hive init` credential chain

In order — first match wins:

1. `--access-key` + `--secret-key` + `--endpoint` flags — zero Cloudflare calls, works with any S3 provider.
2. Reuse from a sibling app: an existing `~/.config/hive/<app>.env` with a matching `CELLD_BUCKET`.
3. Cloudflare path: bucket create (if missing) + S3 key mint. The mint needs `CLOUDFLARE_API_TOKEN` with **API Tokens Edit**; OAuth tokens can't mint keys (Cloudflare's self-managed OAuth catalog has no API Tokens scope).
4. Otherwise: a deep link to the R2 S3-keys page; paste the pair with `--access-key`/`--secret-key`.

### `hive cf login`

Self-managed Cloudflare OAuth (authorization code + PKCE, no client secret). Default public client ID is compiled in; override with `HIVE_CF_CLIENT_ID` (register your own client for production use). Scopes: `argotunnel.write dns.write zone.read workers-r2.write`. Tokens live at `~/.config/hive/auth.json` (0600) and refresh automatically. `--export` prints `export CLOUDFLARE_API_TOKEN=…` instead of saving.

## Run backends

- **process** (default): `celld` as a detached process, logs to `.hive/node.log`, `down` = SIGTERM (celld drains gracefully). Zero moving parts; no supervisor — a reboot leaves the node down until the next `hive up`.
- **docker** (`--docker` or `"backend": "docker"`): node runs as container `hive-<app>` from image `hive/celld:<version>` (built on demand from the official celld release binary), published on `127.0.0.1:<port>`, `--restart unless-stopped`, config drift detected by a label hash → recreate. `docker stop` is the same graceful drain.

One node per fleet bucket prefix, ever. Two nodes on the same `s3://bucket/<app>` prefix produce `DurableObjectRoutingError: owner unreachable`.

## Remote operation

Set `"server": "user@box"` in the hive block and commands proxy to the hive binary on the box over SSH (`ssh box hive <cmd> --local`). No RPC protocol; the same backends run there. `celld deploy` itself is bucket-direct from anywhere — only restart and health checks touch the box. `--local` forces local operation even when `server` is set.

When the server's app directory differs from the local one, set `"dir": "/absolute/path/on/server"` in the hive block. It must be absolute (no `~`). On `deploy`/`up`, hive creates the directory and syncs `package.json` + `wrangler.jsonc` there before running the remote command. `down` and `status` also use `dir` when set.

Box setup: `hive bootstrap` installs hive + celld at `~/.local/bin` on the server (re-run to upgrade). Add docker yourself if wanted. Credentials sync over SSH as `~/.config/hive/<app>.env` (0600).

## exe.dev

`hive exe` drives the exe.dev control plane over its SSH CLI. exe.dev VMs ship with a public hostname, TLS, and an HTTPS front door, so the Cloudflare Tunnel is unnecessary there — the front door is the ingress. Full flow for a fresh VM:

```sh
# hive block: { "port": 8101, "server": "mybot.exe.xyz",
#               "dir": "/home/exedev/apps/mybot", "domain": "mybot.example.com" }
hive exe new mybot        # create the VM, wait for DNS
hive bootstrap            # install hive + celld on it
hive deploy               # ship, restart, health-gate over ssh
hive exe share            # https://mybot.exe.xyz -> :8101, public (or --private)
hive exe domain           # mybot.example.com live: DNS-only CNAME + exe.dev registration
```

`exe domain` uses your Cloudflare credentials to create the CNAME and hard-requires `proxied: false` — exe.dev terminates TLS itself and orange-cloud records break it. Without cf credentials it prints the exact record to create and exits; re-run after creating it. Registration is verified via exe.dev's `domain ls` and retried, since their resolver lags yours.

CI: a stock GitHub-hosted runner runs `hive deploy`; it reaches the box via SSH through the Cloudflare Tunnel (`hive cf tunnel --ssh` adds the `ssh.<domain>` ingress rule; `sshd` bound to loopback, cloudflared as SSH ProxyCommand). Secrets: bucket creds, SSH key, tunnel token.

## Going to production: a box and a domain

Starting from a plain `index.ts` + `wrangler.jsonc`, the whole journey:

```sh
hive init                        # one-time: port + bucket credentials
# point the app at a box in package.json:
#   "hive": { "port": 8101, "server": "user@box", "dir": "/opt/apps/myapp", "domain": "app.example.com" }
hive bootstrap                   # one-time: hive + celld on the box
hive deploy                      # ship, restart, health-gate — over ssh
```

Then pick an ingress — all three end at the same `127.0.0.1:<port>` listener:

- **exe.dev**: `hive exe share && hive exe domain` — the VM's front door does TLS.
- **Cloudflare Tunnel**: `hive cf tunnel`, then install cloudflared on the box with the printed token — zero inbound ports.
- **Anything else**: reverse-proxy `127.0.0.1:8101` yourself (caddy, nginx, Coolify below).

**Updating** is the same command forever: edit `index.ts`, `hive deploy`, done — new version ID, node restart, health gate before it reports ok. CI is a stock GitHub-hosted runner with bucket creds + an SSH key running `hive deploy`.

## Coolify

Coolify replaces hive's node supervision and ingress, not the deploy pipeline. celld loads `deploy/current.json` at startup — the restart is the reload — and a Coolify redeploy only fires on git push, not on a bucket write. So the split is: Coolify runs the container and owns TLS + domain; CI ships the code and restarts the app.

```dockerfile
# Dockerfile — celld node for Coolify
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL https://celld.dev/install.sh | sh
ENV PATH="/root/.local/bin:$PATH"
CMD ["celld", "--bucket", "<bucket>/<app>", "--listen", "0.0.0.0:8101", \
     "--endpoint", "https://<account>.r2.cloudflarestorage.com", "--region", "auto", \
     "--trust-forwarded-headers"]
```

Set the S3 credentials (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, …) in Coolify's env UI, assign the domain there, and point its health check at `/__celld/health`. Note the `0.0.0.0` bind — Coolify's proxy reaches the container over the docker network, unlike hive's loopback posture.

CI deploy step:

```sh
hive deploy --no-restart                 # upload the new version to the bucket
curl -X POST "$COOLIFY_DEPLOY_WEBHOOK"   # restart the container so it loads current.json
```

Two gotchas: never scale past **1 replica** (two nodes on one bucket prefix = `DurableObjectRoutingError`), and the Dockerfile tracks whatever celld.dev serves latest — pin a version if you care.

## Environment variables

| var | purpose |
| --- | --- |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | S3 credentials (fed from the process env or `~/.config/hive/<app>.env`) |
| `S3_ENDPOINT` / `AWS_REGION` | S3 endpoint / region (`auto` for R2) |
| `CELLD_BUCKET` | Fleet bucket; nodes use `s3://<bucket>/<app>` |
| `CLOUDFLARE_API_TOKEN` | Cloudflare auth source of truth — wins over the stored OAuth token |
| `HIVE_CF_CLIENT_ID` | Override the compiled-in OAuth client ID |
| `HIVE_LOGIN_PORT` | Override the OAuth callback port (tests only) |

## Security notes

- Bucket credentials = full fleet control (deployments, cell replicas, ownership records, node leases). Keep them in env vars or `~/.config/hive/*.env` — never in the repo.
- celld's internal/operator listener is unauthenticated. It never leaves loopback; the public listener binds `127.0.0.1` behind your proxy/tunnel.
- Run nodes with `--trust-forwarded-headers` (hive does) so `request.url` keeps the public scheme/host.
