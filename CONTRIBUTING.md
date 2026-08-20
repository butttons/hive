# Contributing

## Ground rules

- **Go, stdlib only.** Zero module dependencies — do not add one without opening an issue first. The artifact is the product: a single static binary with instant cold start.
- Keep files flat in package `main`. When it genuinely hurts, split into `internal/` by backend.
- Functions take `context.Context` explicitly; no globals beyond the command table in `main.go`.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, commands return `error`, `main` prints `hive <cmd>: <err>`.
- No comments except godoc on exported types.
- Agent-first CLI: consistent subcommand grammar, `--json` on every command, `--force` (never `--skip-confirmations`), always explicit whether an operation is local or remote.

## Verify before sending a change

```sh
go vet ./... && CGO_ENABLED=0 go build -o hive . && CGO_ENABLED=0 go test ./...
```

Always `CGO_ENABLED=0` — the binary must stay static, and CGO is broken on some dev hosts.

## Docs

`docs/` is a static site deployed with Wrangler (`cd docs && npx wrangler deploy`). If you change commands, flags, or config shape, update `README.md`, `docs/index.html`, `docs/llms.txt`, and `AGENTS.md` in the same change.

## Release

```sh
git tag vX.Y.Z && git push origin vX.Y.Z
```

CI builds the three binaries (darwin/arm64, linux/amd64, linux/arm64) plus checksums and creates the GitHub release. `https://hive.butttons.dev/setup.sh` installs from the latest release.
