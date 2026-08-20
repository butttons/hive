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

## Command surface (settled; all stubbed in commands.go)

- `hive add` — scaffold a new app project (wrangler.jsonc + index.ts + tsconfig + package.json with hive block), allocate a free port.
- `hive check` — thin compat gate: validate wrangler.jsonc against the celld-legal key list and attempt the deploy path; report yes/no with celld's own errors. Deliberately dumb — celld fails loud, we relay.
- `hive deploy` — typecheck (`tsc -b`) → `celld deploy` → restart the app's node → wait for `/__celld/health` ok. **The restart is the reload** — there is no watch mode or HMR; the loop is `edit → hive deploy → curl` and agents drive it.
- `hive up` / `hive down` — start/stop the node for the current app. Two run backends behind one interface: **launchd** (macOS, plist + launchctl) and **systemd** (Linux). `down` is just SIGTERM — celld drains gracefully (health → 503, finishes in-flight, hands off cells). Idempotent: already-up is a no-op; config drift → restart.
- `hive status` — apps, running nodes, live version IDs (from the bucket), health. `--json`.
- Later: `init` (bucket creds), `login` (OAuth), `tunnel` (ingress), `ui` (local dashboard).

## Ingress: remotely-managed Cloudflare Tunnels (settled)

- celld terminates no TLS and does no host routing. Ingress = Cloudflare Tunnel, **remotely-managed via REST API**: create tunnel, `PUT /cfd_tunnel/<id>/configurations` for ingress rules (one hostname per app domain → `localhost:<port>`), create DNS records via API. The box receives only a tunnel token and runs `cloudflared service install <token>`. No `cloudflared login` browser dance, no cert.pem, no config.yaml on the box.
- Zero inbound ports on the box; celld's public listener stays on 127.0.0.1. The internal/operator listener is unauthenticated — never expose it.
- Run nodes with `--trust-forwarded-headers` so `request.url` keeps public scheme/host.

## Onboarding (settled) and the one open gap

- Cloudflare opened self-managed OAuth to all developers (June 2026): register an OAuth client, `hive login` runs a consent flow with exact scopes (R2 edit, Tunnel edit, DNS edit) → scoped token. This is the entire manual surface — one browser consent.
- Bucket creation, tunnel, DNS: all REST API. Do NOT build on the `cf` CLI — it's a technical preview covering a subset; call the REST API directly.
- **OPEN GAP — verify before `init`:** whether R2 S3 access-key pairs (what celld's AWS credential chain needs: `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`) are mintable via REST API, or still dashboard-only. If dashboard-only, `init` deep-links the user and accepts a paste.

## celld operational facts (verified by doing)

- Installed at `~/.local/bin/celld` (v0.2.1). Docs: https://celld.dev/docs. Repo clone for examples: `~/Work/playground/vendor/celld`.
- **One app = one fleet = one bucket prefix.** `s3://bucket/<app>`; nodes load `deploy/current.json` at startup — restart after every deploy.
- Nodes need `esbuild` on PATH for deploys (0.28.2 installed globally).
- The bucket is the administrative authority: deployments, SQLite replicas, ownership records, node leases, peer-auth secret. Credentials = full fleet control.
- Store requirements: conditional writes + read-after-write consistency. R2 qualifies; B2/MinIO/Spaces do not.
- Operator API on internal listener (`/state`, `POST /shutdown`) is alpha and version-locked — build against it loosely.
- `celld diagnose --bucket ...` enumerates node leases and probes peers.
- TS wrinkle: `DurableObjectNamespace<T>` in @cloudflare/workers-types requires a branded class; use the non-generic `DurableObjectNamespace` unless extending `DurableObject` from `cloudflare:workers`.

## Test environments (live)

- **playground** at `~/Work/playground` — the consumer/test apps. Has its own AGENTS.md. R2 bucket `old-bucket` (EU endpoint, `AWS_REGION=auto`), creds in `env.sh` (source it; never copy creds elsewhere). Apps: `counter` (port 8101, SQLite counter DO) and `wsecho` (port 8102, hibernatable WS DO), TS, deployable via `scripts/deploy.sh`. These apps' package.json files don't have `"hive"` blocks yet — adding them is the first dogfood.
- **remote box** — `user@box`, SSH key auth confirmed working, macOS 26.4.1 arm64, cloudflared NOT installed yet (`brew install cloudflared`). First launchd-backend and tunnel target. Stand-in for "barebones box."

## Current state

Scaffold only. `main.go` (dispatch + usage), `registry.go` (loads app config — real, tested pattern), `commands.go` (all stubs). Builds clean: `go vet ./... && go build .`. No git history yet (`git init` done, nothing committed — ask before committing).

## Build order (next steps, in order)

1. `add` — template scaffolding + port allocation (dogfood by converting playground apps to hive projects).
2. `up`/`down` local backend — plain process spawn + port/health introspection first, launchd second.
3. `deploy` — tsc → celld deploy → restart → health gate. This completes the local loop; everything before the server work.
4. `check` — celld-legal key list validation.
5. launchd backend on the remote box; systemd backend (compile-gated, untested locally).
6. `tunnel` — REST-driven remotely-managed tunnel.
7. `login`/`init` — OAuth + bucket provisioning (resolve the R2 S3-keys API gap first).
8. `ui` — local SPA served from the binary via embed.FS; zero own logic, pure skin over `status --json`.

## Conventions

- Functions take context explicitly; no globals beyond the command table.
- Keep files flat in package `main` until it hurts, then split into `internal/` packages by backend (registry, run, cfapi).
- Error style: wrap with `fmt.Errorf("...: %w", err)`, commands return `error`, main prints `hive <cmd>: <err>`.
- No comments unless JSDoc-equivalent godoc on exported types; no decorative comments.
