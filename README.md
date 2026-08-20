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
| `hive deploy` | `--docker`, `--local` | Typecheck (`tsc -b`) → `celld deploy` → restart the node → wait for `/__celld/health` (30s gate). Prints the version ID |
| `hive deploy all` | `--docker`, `--local`, `--packages` | Deploy every app in the workspace sequentially, continuing past failures |
| `hive up` | `--docker`, `--local` | Start the node. Idempotent; config drift → restart |
| `hive down` | `--local` | Stop the node gracefully (SIGTERM; celld drains in-flight work) |
| `hive status` | `--local`, `--filter` | App config + node state; from a workspace root, show a compact fleet table |
| `hive init` | `--bucket`, `--access-key`, `--secret-key`, `--endpoint`, `--region`, `--jurisdiction`, `--force` | Provision bucket credentials into `~/.config/hive/<app>.env` (0600) |
| `hive cf login` | `--status`, `--export`, `--no-browser` | Cloudflare OAuth consent (PKCE); stores a refreshable token |
| `hive cf tunnel` | `--name`, `--ssh` | Create/sync a remotely-managed Cloudflare Tunnel: ingress rules + DNS, prints the box install command |

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

Box setup: the hive binary at `~/.local/bin/hive`, celld alongside it, docker if wanted. Credentials sync over SSH as `~/.config/hive/<app>.env` (0600). Rebuild + scp the box's hive binary after hive upgrades or it runs stale code.

CI: a stock GitHub-hosted runner runs `hive deploy`; it reaches the box via SSH through the Cloudflare Tunnel (`hive cf tunnel --ssh` adds the `ssh.<domain>` ingress rule; `sshd` bound to loopback, cloudflared as SSH ProxyCommand). Secrets: bucket creds, SSH key, tunnel token.

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
