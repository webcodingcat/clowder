# clowder

A small cluster orchestrator for Docker Compose stacks: replicated volumes (DRBD 9), the real
client IP behind a Floating IP, priorities, fencing through the hoster's API. Go, one module,
two binaries: `clowder` (the CLI; Linux, macOS, Windows) and `clowderd` (the agent; Linux).

Everything in this repo is English. Read these before changing code:

- @DEVELOPING.md – how the code fits together, its rules, how to build and test.
- @STYLE.md – the standards and the rules this project adds to them.

## Commands

All of them run through Docker (Linux, macOS; Git Bash or WSL on Windows). Go and the linters
are not needed on the host.

| Command | What it does |
|---|---|
| `bin/dev lint` | golangci-lint (gofumpt, goimports, the import rule), shellcheck, shfmt, yamllint, `tofu fmt` |
| `bin/dev format` | apply what the formatters can fix |
| `bin/dev test` | the unit tests (seconds, no VMs) |
| `bin/dev build` | `clowderd` for Linux, `clowder` for Linux, macOS and Windows into `dist/` |
| `bin/dev package` | build, then the Debian packages into `dist/` |
| `bin/dev all` | lint, test, package – what CI runs |

## Rules that are easy to break

DEVELOPING.md explains each one.

- The CLI must build on Windows: `cmd/clowder` and `internal/cli` never import a package that
  only works on Linux (Layers).
- `scheduler` and `state` do no I/O; only `agent`, `supervisor` and the drivers change the
  host (Layers).
- The agent never waits on a write to a volume: without quorum DRBD suspends I/O, and such a
  write hangs in the kernel where no timeout ends it (Volumes).
- Errors are wrapped with `%w` and context; logs go through `log/slog` (Logging and errors).

## State of the work

Phase 1a (the foundation on one host). `spike/` is the throw-away spike of phase 0; it goes
away once the live tests of phase 1a replace it.
