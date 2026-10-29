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
| `bin/dev lint` | golangci-lint (gofumpt, goimports, the import rule), shellcheck, shfmt, yamllint, `tofu fmt`, `tofu validate` |
| `bin/dev format` | apply what the formatters can fix |
| `bin/dev test` | the unit tests with the race detector (seconds, no VMs) |
| `bin/dev test-sops` | the tests of `internal/sopsage` against the real sops |
| `bin/dev test-tools` | the golden files of the renderers against the real `nft`, `drbdadm` and `docker-compose` |
| `bin/dev build` | `clowderd` for Linux, `clowder` for Linux, macOS and Windows into `dist/` |
| `bin/dev package` | build, then the Debian packages into `dist/` |
| `bin/dev all` | lint, test, test-sops, test-tools, package – what CI runs |
| `bin/dev live` | the live tests: a host from `examples/hetzner` in an HLab lab of their own, driven by `test/live` (about 15 minutes; `HLAB` points at HLab's `bin/hlab`, needs `/dev/kvm`); `bin/dev live down` removes what a failed run left |

Run the live tests only at the end of a piece of work, on its finished state, and only while
no one else uses the lab `clowder-live`.

## Rules that are easy to break

DEVELOPING.md explains each one.

- The CLI must build on Windows: `cmd/clowder` and `internal/cli` never import a package that
  only works on Linux (Layers).
- `scheduler` and `state` do no I/O; only `agent`, `raftnode`, `supervisor` and the drivers
  (`network`, `storage`, `docker`) change the host (Layers).
- The agent never waits on a write to a volume: without quorum DRBD suspends I/O, and such a
  write hangs in the kernel where no timeout ends it (Volumes). Every host tool runs through
  `internal/command`, with a timeout (Host tools).
- Errors are wrapped with `%w` and context; logs go through `log/slog` (Logging and errors).

## State of the work

Phase 1a, the foundation on one host (DEVELOPING.md, Roadmap), ends with the live tests in
`test/live`.
