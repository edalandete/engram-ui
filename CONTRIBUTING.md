# Contributing to engram-ui

## Hard rules

1. **Never import engram packages.** This codebase only talks to engram via its HTTP API. The `third_party/engram/` submodule exists for building the binary and reading source, never for `import`.
2. **Every API call goes through `internal/client`.** Handlers must not call `net/http` against engram directly.
3. **Mirror the TUI.** When in doubt, copy the TUI screen behavior (`third_party/engram/internal/tui/`). Colors are the exception: this repo has its own palette, defined at the top of `internal/web/static/css/app.css`.
4. **No backwards-compat shims for engram versions.** Bump `internal/version/MinEngram` and fail fast on boot if the running engram is too old.

## Workflow

1. Open issue describing the change.
2. PR references the issue and adds one `type:*` label (`bug`, `feature`, `docs`, `refactor`, `chore`).
3. There is no CI — hosted Actions runners are disabled for this org. Run `make vet test` yourself before pushing.

## Local dev

```powershell
git submodule update --init --recursive
./build-engram.ps1              # build engram binary into internal/launcher/embedded/
go run ./cmd/engram-ui
```
