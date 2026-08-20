# hive — Agent Reference

> **Resuming work? This file is the full handoff. Read it top to bottom before writing code. The design was settled in a long brainstorm with the user — do not relitigate settled decisions without new evidence.**
> Live infrastructure details (hosts, account/tunnel IDs) are in `AGENTS.local.md` — gitignored, never commit it.

## What hive is

`hive` (github.com/butttons/hive, public) is a wrangler-shaped CLI on top of [celld](https://celld.dev/docs) — Deno's self-hosted, distributed Durable Objects daemon. celld runs Workers + DO code on your own machines, with cell state in an S3-compatible bucket you own, but is bare: no ingress, no TLS, no control plane. hive is the missing toolchain: "Bring your existing Worker. hive tells you if it runs on celld, then devs, deploys, and runs it on your own hardware."

## Hard constraints

- **Go, stdlib only.** Zero dependencies — no cobra/viper/any module without asking. The artifact is the product: single static binary, instant cold start, cross-compiled for darwin/arm64 + linux/arm64/amd64 (see `.github/workflows/release.yml`).
- Rejected: Bun/Deno compile (~100MB V8 blob), Zig (pre-1.0 churn), Rust (iteration tax), TS-on-Node (needs Node on bare boxes).
- **Agent-first CLI**: consistent subcommand grammar, `--json` on every command, `--force` (never `--skip-confirmations`), always explicit whether an operation is local or remote.
- **No state files.** hive introspects reality every time: ports, `/__celld/health`, and the bucket's `deploy/current.json`.
- Everything except the app's two config files is generated — plists, tunnel config, env files.

## Config model (settled)

Two files per app project, same directory:

- `wrangler.jsonc` — **pristine**, only the celld-legal subset (celld hard-fails on unknown keys like `routes`). Handed to celld untouched.
- `package.json` — extended with a `"hive"` block for everything celld rejects:

```json
{
  "hive": { "port": 8101, "domain": "app.example.com", "server": "user@box", "dir": "/opt/apps/app", "backend": "docker" }
}
```

`port` is required; `domain`, `server`, `dir`, `backend` optional. Loaded by `registry.go`.

## Monorepo model (settled)

- Workspace discovery from cwd: `pnpm-workspace.yml`/`pnpm-workspace.yaml` `packages:` globs first, then root `package.json` `workspaces` (array or `{packages:[...]}` object). Walk up until a workspace file is found or the git root/filesystem root stops the search.
- A directory counts as a hive app iff its `package.json` contains a `"hive"` block. Glob expansion is minimal stdlib-only: single trailing `/*` (e.g. `apps/*`, `packages/*`); `**` not required.
- Fallback `--packages <glob>` (repeatable or comma-separated) when no workspace file exists.
- `hive deploy all` from a workspace root deploys every app sequentially, continues past failures, prints a per-app summary, exits non-zero if any failed. `--json` returns an array of per-app deploy results (same fields as single deploy, including `version`).
- `hive deploy` bare at a non-app root errors, telling the user to use `hive deploy all`, `--filter`, or cd into an app.
- `--filter <name>` on `deploy`/`status` matches app `package.json` `name` or directory basename; errors listing available apps if no match. With `--filter`, a single app deploys even from the root.
- `hive status` at a workspace root prints a compact fleet table (name, port, server/backend, node up/down, health, version). `hive ui` at a root shows a fleet view. Both honor `--filter` and keep single-app behavior when cwd is an app dir.
- `login` and `tunnel` moved under `hive cf`. Top-level `check` removed.

## Remote model (settled)

- `hive.server` is an **SSH host**. Unset = operate locally. Set = the CLI proxies to the hive binary on the box: `ssh <server> ~/.local/bin/hive <cmd> --local`. No RPC protocol; `--local` forces local.
- `hive.dir` is the absolute app directory on the server. When empty, the local `app.Dir` is sent verbatim (works when the box shares the filesystem layout, e.g. the mac mini). When set, it must start with `/` (no tilde expansion). `down` and `status` use it as the remote cwd.
- On `deploy`/`up`, after syncing `~/.config/hive/<app>.env`, hive also `mkdir -p <dir>` and `scp package.json wrangler.jsonc` to `<dir>` before issuing the remote command. `celld deploy` is bucket-direct from anywhere; only restart + health checks touch the box.
- CI = stock GitHub-hosted runner running `hive deploy`, reaching the box via SSH through the Cloudflare Tunnel (`hive cf tunnel --ssh` adds the `ssh.<domain>` ingress rule; `sshd` loopback-only, cloudflared as ProxyCommand). Secrets: bucket creds, SSH key, tunnel token.

## Commands and backends

All implemented and verified live. User-facing reference: README.md / hive.butttons.dev.

- `add` `deploy` `up` `down` `status` `init` `env` `bootstrap` `cf` `exe` `ui`. `env` prints the effective app env (shell-sourcable; `--tunnel` adds TUNNEL_TOKEN) — feeds compose `.env` and CI secrets.
- `bootstrap` = install/upgrade hive + celld at `~/.local/bin` on `hive.server` over ssh. The bare-box error path in `deploy`/`up` points at it.
- `deploy` = typecheck (`tsc -b`) → `celld deploy` → restart node → 30s `/__celld/health` gate. **The restart is the reload** — no watch mode or HMR.
- Backends behind one interface; `down` is SIGTERM either way (celld drains gracefully). Idempotent; config drift → restart.
  - **process** (default): `celld` detached, log `.hive/node.log`. No supervisor — a reboot leaves the node down.
  - **docker** (`--docker` / `"backend": "docker"`): container `hive-<app>`, image `hive/celld:<version>` built on demand, `127.0.0.1:<port>`, `--restart unless-stopped`, 0600 `--env-file`, label-hash drift detection. launchd/systemd were cut (git history has them); docker subsumes them.
- Deployment targets = celld's: linux/amd64, linux/arm64, darwin/arm64. A bare box needs exactly `hive` + `celld` at `~/.local/bin`, plus docker if wanted.

## Provider model (settled)

- **Provider-agnostic core.** Hard requirements: S3 credentials (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`S3_ENDPOINT`/`AWS_REGION`/`CELLD_BUCKET`) + an SSH-reachable box. Everything works with exactly that.
- **Cloudflare is optional convenience**: `cf login` (OAuth), `init` (R2 provisioning), `cf tunnel` (ingress). Without Cloudflare, users set the env vars and terminate TLS however they want — hive prescribes no ingress.
- **Env vars win.** `CLOUDFLARE_API_TOKEN` beats `~/.config/hive/auth.json`; per-app `~/.config/hive/<app>.env` (0600) feeds celld. All credential files are 0600 and never committed.
- `init` cred chain: `--access-key/--secret-key/--endpoint` flags (zero CF calls) → reuse from a sibling app's env file (matching `CELLD_BUCKET`) → CF mint → deep-link paste fallback.
- **exe.dev is optional convenience** under `hive exe` (`new`/`share`/`domain`), driven over its SSH CLI (`ssh exe.dev <cmd> --json` — every subcommand takes `--json`; parse it, never scrape stdout). exe.dev VMs ship a public hostname + TLS + HTTPS front door, so the CF tunnel is unnecessary there. `exe new` is idempotent (checks `ls --json` first). `exe domain` upserts the CNAME via cf creds with `proxied: false` (exe.dev terminates TLS; orange-cloud breaks it), then `domain add`. Gotcha: `domain add` can fail on stdout with exit 0 and their resolver lags ours — always verify via `domain ls --json` and retry.

## Cloudflare specifics

- OAuth (`hive cf login`): authorization code + PKCE (S256), auth method `none`, redirect `http://127.0.0.1:8976/callback` (override `HIVE_LOGIN_PORT` for tests). Scopes: `argotunnel.write dns.write zone.read workers-r2.write`. `account.read` is NOT allowlistable on self-managed clients (consent fails `invalid_scope`); zone listing covers account discovery. Default client ID compiled into login.go; override `HIVE_CF_CLIENT_ID`.
- R2 S3 keys ARE mintable via REST (user API token with `Workers R2 Storage Bucket Item Write`; key id = token `id`, secret = SHA-256 hex of token `value`) but NOT with an OAuth token — no API Tokens scope in the OAuth catalog. So mint needs `CLOUDFLARE_API_TOKEN` with API Tokens Edit; OAuth users get the paste path.
- Tunnel ingress: remotely-managed via REST (`PUT /cfd_tunnel/<id>/configurations`, DNS via API). The box gets only a tunnel token: `cloudflared service install <token>`. Zero inbound ports; celld's public listener stays on 127.0.0.1; the operator listener is unauthenticated — never expose it. Nodes run with `--trust-forwarded-headers`.
- Do NOT build on the `cf` CLI — technical preview, subset coverage. Call the REST API directly.

## celld operational facts (verified)

- **One app = one fleet = one bucket prefix** (`s3://bucket/<app>`). Nodes load `deploy/current.json` at startup — restart after every deploy.
- **Only ONE node per prefix may run.** Two nodes → `DurableObjectRoutingError: owner unreachable`. `hive down --local` before testing remote.
- The bucket is the administrative authority (deployments, replicas, ownership, leases, peer secret). Credentials = full fleet control.
- Store requirements: conditional writes + read-after-write consistency. R2/S3/Tigris qualify; B2/MinIO/Spaces do not.
- Nodes need `esbuild` on PATH for deploys. Verified at celld v0.2.1.
- Operator API (`/state`, `POST /shutdown`) is alpha and version-locked — build against it loosely.
- `celld deploy` prints `Current Version ID: <16-hex>`; `hive deploy --json` surfaces it as `"version"`.
- TS wrinkle: use non-generic `DurableObjectNamespace` unless extending `DurableObject` from `cloudflare:workers`.

## Box gotchas (verified)

- macOS non-login ssh PATH lacks Homebrew (`brew`, `docker`) — hive's proxied commands work because `~/.local/bin` is on it; Homebrew tools need `zsh -lc` or absolute paths.
- Refresh a box's hive binary after hive changes (`CGO_ENABLED=0 GOOS=<goos> GOARCH=<goarch> go build` + scp) or it runs stale code.
- macOS `cloudflared service install <token>` (non-root) = **user** LaunchAgent, runs only while logged in; over ssh needs `launchctl kickstart gui/<uid>/com.cloudflare.cloudflared` the first time.

## Conventions

- Functions take context explicitly; no globals beyond the command table.
- Keep files flat in package `main` until it hurts, then split into `internal/` by backend (registry, run, cfapi).
- Errors: wrap with `fmt.Errorf("...: %w", err)`, commands return `error`, main prints `hive <cmd>: <err>`.
- No comments except godoc on exported types.
- Verify with `go vet ./... && CGO_ENABLED=0 go build -o hive . && CGO_ENABLED=0 go test ./...` — CGO is broken on the dev host, ALWAYS `CGO_ENABLED=0`.
- Release: `git tag vX.Y.Z && git push origin vX.Y.Z` — CI builds the three binaries + checksums and creates the GitHub release. `docs/setup.sh` installs from `releases/latest/download`.
