# hive — Agent Reference

> **Resuming work? This file is the full handoff. Read it top to bottom before writing code. The design below was settled in a long brainstorm with the user — do not relitigate settled decisions without new evidence.**

## What hive is

`hive` (github.com/butttons/hive) is a wrangler-shaped CLI on top of [celld](https://celld.dev/docs) — Deno's self-hosted, distributed Durable Objects daemon. celld runs Workers + DO code on your own machines, with cell state replicated to an S3-compatible bucket you own. celld is a great primitive but bare: no ingress, no TLS, no multi-app story, no control plane, one deployment per fleet. hive is the missing toolchain.

**The pitch (maybe public later, internal for now):** "Bring your existing Worker. hive tells you if it runs on celld, then devs, deploys, and runs it on your own hardware."

The user is a solo founder, TS-first, iterates with agents. Internal toolchain now; design the command surface and config keys as if public later, but spend zero effort on polish/marketing.

## Language and hard constraints

- **Go, stdlib only.** Zero dependencies. Do not add cobra, viper, or any module without asking. The artifact is the product: single static binary (~10MB), instant cold start, cross-compiled for darwin/arm64 + linux/arm64/amd64.
- Rejected and why: Bun/Deno compile (embed V8 → ~100MB blob), Zig (stdlib TLS/HTTP/JSON too young for a glue CLI, pre-1.0 churn), Rust (iteration-velocity tax), TS-on-Node (needs Node on barebones boxes, startup friction).
- **Agent-first CLI conventions** (stolen from Cloudflare's `cf` CLI announcement): consistent subcommand grammar, `--json` output on every command, `--force` (never `--skip-confirmations`), always explicit whether an operation is local or remote. Agents are the primary user.
- **No state files.** hive introspects reality every time: ports, `/__celld/health`, `launchctl`/`systemctl`, and the bucket's `deploy/current.json`. A CLI that keeps its own state starts lying.
- **The registry is the only hand-edited thing.** Everything else — plists, units, tunnel config — is generated from it.

## Config model (settled)

Two files per app project, same directory:

- `wrangler.jsonc` — **pristine**, only the celld-legal subset (celld hard-fails on unknown keys like `routes`). This is the deploy contract handed to celld untouched.
- `package.json` — extended with a `"hive"` block for everything celld rejects:

```json
{
  "hive": { "port": 8101, "domain": "counter.example.com", "server": "user@box" }
}
```

`registry.go` already loads this. `port` is required; `domain` and `server` optional (local-only apps skip them).

## Remote model / server field (settled)

- `hive.server` is an **SSH host** (`user@host`).
- No `server` = operate locally.
- `server` set = the local CLI proxies to the hive binary installed on the box: `ssh <server> hive <cmd> <args> --local`. One implementation of the run backends, no RPC protocol.
- `celld deploy` itself is bucket-direct from anywhere; only node restart + health checks touch the box.
- CI = stock GitHub-hosted runner running `hive deploy`. The runner reaches the box via SSH through the Cloudflare Tunnel: one extra ingress rule (`ssh.<domain>` → `ssh://localhost:22`), `sshd` bound to loopback only, and `cloudflared` as an SSH ProxyCommand. Secrets: bucket creds, SSH key, tunnel token. Future simplification: celld's alpha `POST /shutdown` behind an Access-gated tunnel hostname + keepalive restart could replace SSH later; not building it now.

## Command surface (settled; all stubbed in commands.go)

- `hive add` — scaffold a new app project (wrangler.jsonc + index.ts + tsconfig + package.json with hive block), allocate a free port.
- `hive check` — thin compat gate: validate wrangler.jsonc against the celld-legal key list and attempt the deploy path; report yes/no with celld's own errors. Deliberately dumb — celld fails loud, we relay.
- `hive deploy` — typecheck (`tsc -b`) → `celld deploy` → restart the app's node → wait for `/__celld/health` ok. **The restart is the reload** — there is no watch mode or HMR; the loop is `edit → hive deploy → curl` and agents drive it.
- `hive up` / `hive down` — start/stop the node for the current app. Three run backends behind one interface:
  - **process** (default local dev): spawn `celld` detached, log to `.hive/node.log`, introspect port + `/__celld/health`, `down` = SIGTERM.
  - **launchd** (macOS server): generated plist, `launchctl bootstrap/bootout`, drift → restart.
  - **systemd** (Linux server): generated unit, `systemctl --user start/stop`, drift → restart.
  Backend selection: `hive up` uses **process** by default; `hive up --daemon` uses launchd on macOS / systemd on Linux. `down` is SIGTERM — celld drains gracefully (health → 503, finishes in-flight, hands off cells). Idempotent: already-up is a no-op; config drift → restart.
- `hive status` — current app config + node state (running, pid, backend, health). `--json` is the primary interface. Live version from the bucket is shown only when cheap to read; otherwise "unknown".
- Later: `init` (bucket creds), `login` (OAuth), `tunnel` (ingress), `ui` (local dashboard).

## Provider model (settled)

- **hive's core is provider-agnostic.** The hard requirements for running an app are: S3-compatible credentials (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`S3_ENDPOINT`/`AWS_REGION`/`CELLD_BUCKET`) and a box (Linux/macOS) reachable over SSH. Everything in `add`/`check`/`up`/`down`/`deploy`/`status` works with exactly that — no Cloudflare anything.
- **Cloudflare is an optional convenience provider**, not a dependency: `login` (OAuth consent), `init` (R2 bucket + S3 key provisioning), and `tunnel` (ingress) automate the Cloudflare path. Users without Cloudflare set the S3 env vars themselves and terminate TLS however they want (caddy, nginx, their own tunnel) — hive does not prescribe ingress for them.
- **Env vars are the auth source of truth.** `CLOUDFLARE_API_TOKEN` in the environment wins over anything stored; `hive login` merely manages `~/.config/hive/auth.json` as a cache and can emit `export CLOUDFLARE_API_TOKEN=...` via `--export`. Same rule for celld: the process environment (or the per-app `~/.config/hive/<app>.env` file) feeds everything.

## Ingress: remotely-managed Cloudflare Tunnels (the Cloudflare path)

- celld terminates no TLS and does no host routing. The Cloudflare ingress path = remotely-managed tunnel via REST API: create tunnel, `PUT /cfd_tunnel/<id>/configurations` for ingress rules (one hostname per app domain → `localhost:<port>`), create DNS records via API. The box receives only a tunnel token and runs `cloudflared service install <token>`. No `cloudflared login` browser dance, no cert.pem, no config.yaml on the box.
- Zero inbound ports on the box; celld's public listener stays on 127.0.0.1. The internal/operator listener is unauthenticated — never expose it.
- Run nodes with `--trust-forwarded-headers` so `request.url` keeps public scheme/host.

## Onboarding (settled) and the one open gap

- Cloudflare opened self-managed OAuth to all developers (June 2026): register an OAuth client, `hive login` runs a consent flow with exact scopes (R2 edit, Tunnel edit, DNS edit) → scoped token. This is the entire manual surface — one browser consent.
- Bucket creation, tunnel, DNS: all REST API. Do NOT build on the `cf` CLI — it's a technical preview covering a subset; call the REST API directly.
- R2 S3 key pairs ARE mintable via REST: create a user API token (POST `/user/tokens`) with the `Workers R2 Storage Bucket Item Write` permission group scoped to the bucket; Access Key ID = the token's `id`, Secret Access Key = SHA-256 hex of the token's `value`. Requires the OAuth client to also allowlist the API Tokens edit scope; without it, `init` deep-links the R2 API tokens page and accepts `--access-key`/`--secret-key`.

## Cloudflare OAuth (`hive login`)

- Default public client ID: `d6188eb87e7198f8f9fd8ef81abc6539`. Override with `HIVE_CF_CLIENT_ID`.
- Authorization endpoint: `https://dash.cloudflare.com/oauth2/auth`. Token endpoint: `https://dash.cloudflare.com/oauth2/token`.
- Redirect URI: `http://127.0.0.1:8976/callback`. Port is hardcoded; override with `HIVE_LOGIN_PORT` only for tests.
- Flow: OAuth 2.0 Authorization Code + PKCE (`S256`), token endpoint authentication method `none`, no client secret.
- Requested scopes: `argotunnel.write dns.write zone.read workers-r2.write`. Note: `account.read` is not allowlistable on self-managed clients (consent fails with `invalid_scope`); zone listing covers account discovery. The R2 scope (`workers-r2.write`) comes from the cfui project's live OAuth usage ([cfui README](https://github.com/dockers-x/cfui)).
- Token storage: `~/.config/hive/auth.json`, written `0600`. Stores `access_token`, `refresh_token`, `expires_at`, and `scope`. Token values are never logged. `hive login --export` prints `export CLOUDFLARE_API_TOKEN=...` instead of saving.
- `loadToken()` refreshes an expired token automatically when a `refresh_token` is present. `hive login --status` reports validity, expiry, and granted scopes.

## celld operational facts (verified by doing)

- Installed at `~/.local/bin/celld` (v0.2.1). Docs: https://celld.dev/docs. Repo clone for examples: `~/Work/playground/vendor/celld`.
- **One app = one fleet = one bucket prefix.** `s3://bucket/<app>`; nodes load `deploy/current.json` at startup — restart after every deploy.
- Nodes need `esbuild` on PATH for deploys (0.28.2 installed globally).
- The bucket is the administrative authority: deployments, SQLite replicas, ownership records, node leases, peer-auth secret. Credentials = full fleet control.
- Store requirements: conditional writes + read-after-write consistency. R2 qualifies; B2/MinIO/Spaces do not.
- Operator API on internal listener (`/state`, `POST /shutdown`) is alpha and version-locked — build against it loosely.
- `celld diagnose --bucket ...` enumerates node leases and probes peers.
- TS wrinkle: `DurableObjectNamespace<T>` in @cloudflare/workers-types requires a branded class; use the non-generic `DurableObjectNamespace` unless extending `DurableObject` from `cloudflare:workers`.

## Deploy implementation notes (from dogfooding)

- `celld deploy` invocation, equivalent to `scripts/deploy.sh`: run from the app directory with `celld deploy --bucket <CELLD_BUCKET>/<app> --endpoint <S3_ENDPOINT> --region <AWS_REGION>`. Credentials come from the environment (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `S3_ENDPOINT`). Stream stdout/stderr; on failure relay the output unchanged.
- Version ID: celld prints `Current Version ID: <16-char hex>` on success (e.g. `d733f37ce356fc35`). `hive deploy --json` surfaces this as `"version"`.
- Health gate: after restart, poll `GET 127.0.0.1:<port>/__celld/health` until `{"ok":true}` or a 30 s timeout. On timeout, error and point at `.hive/node.log`.
- Typecheck: run `tsc -b` in the app directory; use `node_modules/.bin/tsc` if found (walking up from the app dir for monorepo layouts), else `tsc` on PATH. Skip with a note if no `tsconfig.json`.
- Remote restart: when `hive.server` is set, `celld deploy` still runs locally (bucket-direct); only restart+health proxy over `ssh <server> hive down --local [--daemon]` then `ssh <server> hive up --local [--daemon]`.

## Test environments (live)

- **playground** at `~/Work/playground` — the consumer/test apps. Has its own AGENTS.md. R2 bucket `old-bucket` (EU endpoint, `AWS_REGION=auto`), creds in `env.sh` (source it; never copy creds elsewhere). Apps: `counter` (port 8101, SQLite counter DO) and `wsecho` (port 8102, hibernatable WS DO), TS, deployable via `scripts/deploy.sh`. `counter/package.json` now has a `"hive"` block; `wsecho` does not yet.
- **remote box** — `user@box`, SSH key auth confirmed working, macOS 26.4.1 arm64, cloudflared NOT installed yet (`brew install cloudflared`). First launchd-backend and tunnel target. Stand-in for "barebones box."

## Current state

`add`, `up`/`down`, `deploy`, `check`, `login`, and `tunnel` are implemented; `add` through `deploy` are dogfooded against `playground/apps/counter`, `login` and `tunnel` verified against the live Cloudflare account. `init` is implemented but its token-minting path is blocked on the API Tokens OAuth scope (bucket creation verified; `mybucket` exists). `ui` is still a stub. Builds clean: `go vet ./... && CGO_ENABLED=0 go build `.

## Build order (next steps, in order)

1. `add` — template scaffolding + port allocation (dogfood by converting playground apps to hive projects).
2. `up`/`down` local backend — ✅ process backend done; launchd/systemd code written.
3. `deploy` — ✅ tsc → celld deploy → restart → health gate. Dogfooded end-to-end against `counter`.
4. `check` — ✅ celld-legal key list validation + deploy-path dry-run.
5. launchd backend on the remote box (dry-run, not done); systemd backend (code written, untested locally).
6. `tunnel` — REST-driven remotely-managed tunnel.
7. `init` — bucket provisioning (resolve the R2 S3-keys API gap first).
8. `ui` — local SPA served from the binary via embed.FS; zero own logic, pure skin over `status --json`.

## Conventions

- Functions take context explicitly; no globals beyond the command table.
- Keep files flat in package `main` until it hurts, then split into `internal/` packages by backend (registry, run, cfapi).
- Error style: wrap with `fmt.Errorf("...: %w", err)`, commands return `error`, main prints `hive <cmd>: <err>`.
- No comments unless JSDoc-equivalent godoc on exported types; no decorative comments.
