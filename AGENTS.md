# AGENTS.md — engram-ui

(`CLAUDE.md` just points here.)

## Memory-first

Engram is this project's memory too. Before reading source:

```
mem_current_project     → expect "engram-ui"
mem_search "<topic>"    → past decisions
mem_context             → recent sessions
```

Save decisions and gotchas as you go, with topic keys (`architecture/*`,
`pattern/*`, `decision/*`).

## Hard rules

1. **Never `import` engram packages.** `third_party/engram/` is for building
   the binary and reading source. Nothing else.

2. **Four ways out of this process, no fifth.** Handlers never call `net/http`
   or `os/exec` directly.

   | Boundary | Package | For |
   |---|---|---|
   | Local HTTP | `internal/client` | Every read and write against the local engram. Default. |
   | CLI exec | `internal/engramcli` | Cloud sync ops engram exposes *only* as subcommands — local engram serves a read-only `GET /sync/status` and has no HTTP trigger for push/pull/enroll. |
   | Cloud HTTP read | `internal/cloudclient` | Manifest + chunks from the cloud server. Read-only, used solely by `cloudbridge` for selective pull. |
   | git exec | `internal/gitcli` | The system git, driven by `gitbridge`. Non-interactive by construction (`GIT_TERMINAL_PROMPT=0`, SSH `BatchMode`) so a prompt can never hang a request. |

3. **Bump `internal/version/MinEngram` when you rely on a new engram field or
   endpoint** — and weigh it first, since it makes boot fail for anyone on an
   older engram. Emulating a gap here is often cheaper; see
   `mem_search "roadmap state"`.

4. **Mirror TUI behaviour** (`third_party/engram/internal/tui/`) for screens.
   Colors are this repo's own — see the header of
   `internal/web/static/css/app.css`.

5. **Templates are embedded.** The user rebuilds and restarts by hand; ask
   before touching their processes.

## Quick map

```
cmd/engram-ui/main.go           Entry: launcher → version check → web server
internal/client/                Typed HTTP wrapper for LOCAL engram. Single source of API truth.
internal/engramcli/             CLI exec wrapper for cloud-sync ops + local import.
internal/cloudclient/           Read-only HTTP client to the CLOUD server. Used only by cloudbridge.
internal/cloudbridge/           Selective pull: filter cloud chunks by created_by → temp .engram/ → LocalImport.
internal/gitcli/                Exec wrapper for the system git. Only place that execs git.
internal/gitbridge/             Shared-repo sync: one folder per project per repo; several repos, each project publishes to one (repos.go). Owns the publish order and the manifest union.
internal/web/handlers/          HTTP routes. sync.go = cloud panel; share.go = shared-repo panel.
internal/web/static/css/app.css Palette (this repo's own — see the file header).
internal/launcher/              Engram subprocess lifecycle. BinPath() feeds engramcli.
internal/config/                Flags + env.
internal/version/               Build info + MinEngram.
third_party/engram/             git submodule, read-only.
build.ps1 / build-engram.ps1 / bump-engram.ps1
release.ps1                     Cuts a release from this machine (build + gh release create).
```

No CI. Hosted Actions runners are disabled for this org, so every workflow
failed on every push; the `.github/workflows/` folder was removed rather than
left rotting. Consequences: run `make vet test` before pushing yourself, and
run `make bump-engram` by hand — that used to be a weekly job.

## Sources of truth (when memory is silent)

No `docs/` folder, deliberately: a status file nobody updates is worse than
none, and every section of the last one had rotted. Rationale lives in Engram,
decisions next to the code they govern.

| Need | Open |
|---|---|
| Why this repo exists, boundaries | `README.md`, this file |
| Why a separate process / HTTP, the `MinEngram` contract | `mem_search "why separate process HTTP"` |
| What is built, what is left | `mem_search "roadmap state"` |
| Why a workaround exists | The package doc of the package doing it — `client`, `gitbridge`, `engramcli` each carry theirs |
| Engram API endpoints | `third_party/engram/internal/server/server.go` (read-only) |
| Engram TUI to mirror | `third_party/engram/internal/tui/` (read-only) |
