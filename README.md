# engram-ui

Web UI for [Engram](https://github.com/Gentleman-Programming/engram). Same
screens as the TUI, in a browser, built and released independently of engram.

> **Personal copy** of Rafa Alonso's engram-ui v0.1.16 (MIT, see `LICENSE`),
> imported without its history.

> **Design rule.** This repo never imports engram packages. It talks to engram
> over the local HTTP API, and for the few operations engram only exposes as
> CLI subcommands, by running the binary. Engram is a dependency, not a library.

That is not a preference: engram's `internal/*` packages are unreachable from
outside its module, so the HTTP API is the only supported boundary anyway. It
also means the two projects ship on their own schedules.

## How it fits together

```
engram-ui  :7438          ← you point a browser here
   │
   │ HTTP :7437
   ▼
engram serve              ← started as a subprocess by default
   │
   ▼
~/.engram/engram.db       (SQLite, FTS5)
```

`third_party/engram/` is a git submodule pinning the commit the embedded engram
binary is built from. Read-only: for building and for reading engram's source,
never for Go imports.

`internal/version.MinEngram` is the minimum engram version this UI needs. Boot
hits `/health` and fails fast against anything older.

## Quick start

```powershell
git clone --recurse-submodules https://github.com/edalandete/engram-ui.git
cd engram-ui
./build.ps1                  # engram binary from the submodule, then engram-ui
./bin/engram-ui.exe          # opens http://127.0.0.1:7438
```

`make build` does the same through pwsh.

## Dev

```powershell
go run ./cmd/engram-ui --spawn-engram=false
```

Assumes an `engram serve` already running. Flags (all with `ENGRAM_UI_*` /
`ENGRAM_*` env equivalents, see `internal/config`): `--engram-url`,
`--listen`, `--spawn-engram`, `--open-browser`, `--engram-token`,
`--cloud-token`.

Templates are embedded, so rebuild to see template changes.

## Layout

```
cmd/engram-ui/main.go      Entry: launcher → version check → web server
internal/client/           Typed HTTP client for local engram. Single source of API truth.
internal/engramcli/        Execs the engram binary for cloud sync ops that have no HTTP trigger.
internal/cloudclient/      Read-only HTTP reads from the cloud server (manifest + chunks).
internal/cloudbridge/      Selective pull from cloud, filtered by contributor.
internal/gitcli/           Execs the system git. Non-interactive.
internal/gitbridge/        Shared-repo sync: share memories through a git repo.
internal/web/              Handlers, templates, static (HTMX + a little JS).
internal/launcher/         Engram subprocess lifecycle + the embedded binary.
internal/config/           Flags and env.
internal/version/          Build info + MinEngram.
third_party/engram/        git submodule, read-only.
*.ps1                      build, build-engram, bump-engram.
```

## Per-repository publishing settings

Optional, hand-edited, in the engram data directory (`~/.engram/engram-ui-repos.json`),
keyed by the remote URL exactly as `git remote get-url origin` prints it in the clone:

```json
{
  "repos": {
    "git@github-personal:me/engram-memories.git": {
      "created_by": "me",
      "blocklist": ["work-account", "acme\\.com", "slack"]
    }
  }
}
```

- `created_by`: the name chunks for this repository are signed with, instead of
  the OS account.
- Any entry turns on screening before publish: every observation must belong to
  the project, must not be `personal` scope, and must not match a `blocklist`
  pattern (Go regular expressions, case-insensitive). A refused commit is held
  back and listed; fix the observations and publish again, or use **Publish
  held-back commit anyway**.
- Repositories without an entry behave as before. An unreadable file stops the
  publish instead of skipping the screening.

## Status

Built: dashboard, observations, timeline, sessions, conflicts, graph, projects,
prompts, topic-keys, manual save, diff, export (Markdown / CSV / JSON /
Obsidian vault), cloud sync, and shared-repo sync.

Left: per-observation "do not share" control. Doable today for the shared-repo
transport; out of reach for cloud until engram grows a per-observation sync flag.

There is no `docs/` folder, on purpose — a status file nobody updates is worse
than none. Design rationale lives in Engram itself (`mem_search "roadmap
state"`), and every workaround is explained in the package doc of the package
doing it.

## License

MIT, same as engram. See `LICENSE`.
