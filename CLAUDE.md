# clowder

A small cluster orchestrator that runs stacks with Docker Compose: replicated volumes (DRBD
9), the real client IP behind a Floating IP, priorities, fencing through Hetzner Cloud's
API (clowder runs on Hetzner Cloud with a Hetzner private network only). Go, one module,
two binaries: `clowder` (the CLI; Linux, macOS, Windows) and `clowderd` (the agent; Linux).

Everything in this repo is English. Read these before changing code:

- @DEVELOPING.md – how the code fits together, its rules, how to build and test; it points
  to the part of `docs/developing/` about the package you change.
- @STYLE.md – the standards and the rules this project adds to them.

A change that users see also updates their documentation: the README,
[docs/operating.md](docs/operating.md) (with its Known limits) and
[docs/resources.md](docs/resources.md).

## Commands

All of them run through Docker (Linux, macOS; Git Bash or WSL on Windows). Go and the linters
are not needed on the host.

| Command | What it does |
|---|---|
| `bin/dev lint` | golangci-lint (gofumpt, goimports, the import rule), shellcheck, shfmt, yamllint, `tofu fmt`, `tofu validate` |
| `bin/dev format` | apply what the formatters can fix |
| `bin/dev test` | the unit tests with the race detector (seconds, no VMs) |
| `bin/dev test-tools` | the golden files of the renderers against the real `nft`, `drbdadm` and `docker-compose` |
| `bin/dev build` | `clowderd` for Linux, `clowder` for Linux, macOS and Windows into `dist/` |
| `bin/dev package` | build, then the Debian packages into `dist/` |
| `bin/dev all` | lint, test, test-tools, package – what CI runs |
| `bin/dev live` | the live tests: clusters from `examples/hetzner` in up to three HLab labs at once, each with a group of the scenarios of `test/live` (about 45 minutes with three labs and the image in place; `HLAB` points at HLab's `bin/hlab`, needs `/dev/kvm`); a cluster has a layout: `three` hosts with every role (default), `roles` (three control-plane hosts, two workers) or `single` (one host): `live up [--lab <n>] [--layout <layout>]` makes the hosts of one lab (from a prepared image, `live image`, made when missing or old), `live run [--lab <n>] [-run <test>]` runs scenarios on them, `live down [--lab <n>]` removes them; `bin/dev live` does all three in each lab; `bin/dev live release <tag>` tests a published release (Shipping, Release checklist) |

Run the live tests only at the end of a piece of work, on its finished state. A lock keeps
two runs on a lab apart; after a failure, `bin/dev live run --lab <n> -run <test>` runs one
scenario again on the hosts that stay.

## Rules that are easy to break

DEVELOPING.md explains each one.

- The CLI must build on Windows: `cmd/clowder` and `internal/cli` never import a package that
  only works on Linux (Layers).
- `scheduler` and `state` do no I/O; only `clowderd` and its packages, `supervisor`,
  `raftnode`, `datakey`, `command` and the drivers (`network`, `storage`, `docker`,
  `ingress`) change the host (Layers).
- `internal/clowderd` has a package per command, each with `NewCommand`; the commands and
  the daemon share only `clowderd/hostfiles` and `clowderd/service` (init and join also
  `clowderd/hostsetup`) and never import each other, which depguard checks (The two
  binaries).
- The agent never waits on a write to a volume: without quorum DRBD suspends I/O, and such a
  write hangs in the kernel where no timeout ends it (Volumes). Every host tool runs through
  `internal/command`, with a timeout (Host tools).
- Errors are wrapped with `%w` and context; logs go through `log/slog` (Logging and errors).

## State of the work

[docs/roadmap.md](docs/roadmap.md) says how far each phase is, and nothing else does.
[docs/live-tests.md](docs/live-tests.md) describes the live tests.
