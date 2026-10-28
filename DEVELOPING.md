# Developing clowder

How the code fits together and which rules hold it together. [STYLE.md](STYLE.md) has the
coding standards, `bin/dev` the commands.

## Getting started

You need Bash and Docker. On Windows, use Git Bash or WSL; Docker Desktop with WSL2 works.

```bash
bin/dev test     # unit tests, seconds
bin/dev lint     # all style checks
bin/dev build    # binaries into dist/
dist/linux-amd64/clowder version
```

Go is not needed on the host: `bin/dev` runs everything in pinned images (`golang:1.27.3`,
`golangci/golangci-lint:v2.15.0`, ...) and keeps Go's caches in the Docker volume
`clowder-gocache`. If you do have Go 1.27 installed, `go test ./...` works the same.

### Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs (it needs
`/dev/kvm`). clowder only uses its interface: `hlab env` prints the API endpoint, the token,
the Storage Box access and the Docker network of a running lab. Point `HLAB` at its wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
spike/run.sh up      # phase 0: a lab "clowder" with three hosts
```

The live tests of phase 1a replace `spike/`; they live in `test/live` behind the build tag
`live`.

## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads the cluster directory, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (systemd service `clowderd`) and the host commands `check`, `init`, `join`, `recover` |

`cmd/clowderd` has the build constraint `linux`. Both get their version from
`internal/buildinfo.Version`, which `bin/dev build` stamps from the Git tag (or
`0.0.0~dev+<commit>`).

## Layers

The package layout is planned for the whole of v1; packages appear as the phases add them.

- **Portable**: `cli`, `api`, `clusterdir`, `pki`, `compose`, `buildinfo`, `logging`. The CLI may
  import only these. `.golangci.yml` (depguard, rule `cli-portable`) refuses direct imports
  of the Linux-only packages in `cmd/clowder` and `internal/cli`; the cross build for Windows
  and macOS in `bin/dev build` catches indirect ones.
- **Pure logic, no I/O**: `scheduler`, `state`. They are tested with table tests only.
- **Decides**: `controller` (the leader's loops) writes Raft commands. It talks to external
  services only the leader uses (hoster API, ACME, Git remotes, alert channels), but never
  changes the host.
- **Changes the host**: `agent`, `supervisor` and the drivers `network`, `storage`, `docker`,
  `ingress`.

## Volumes

Without quorum, DRBD suspends I/O (`on-no-quorum suspend-io`): a write then hangs in the
kernel in state D, and neither a timeout nor a signal ends it. So the agent never writes to a
volume in its own loops and never waits for a process that does. Work on volumes (freeze,
snapshot, copy) runs in separate processes that the agent watches for progress. Health
reports never depend on a volume.

## Logging and errors

- `internal/logging` sets up `log/slog`: JSON on stderr for the agent (systemd passes it to
  journald), text for the CLI.
- Errors are wrapped with `%w` and say what was tried; the CLI prints the chain once at the
  top (`clowder: ...`).

## Packages and units

`packaging/` holds the nfpm configurations, the systemd units and the post-install script.

- `clowderd` depends on Debian packages and on `drbd-dkms` and `drbd-utils` from LINBIT's
  public repository (dist `proxmox-9`); the host's APT sources must list it. `coccinelle`
  lets the DKMS build run offline.
- `clowderd.service`: `Type=notify`, `WatchdogSec=15`, `Restart=always`, no reboot on repeated
  failures, `KillMode=process` (containers keep running).
- `clowder-tasks.slice`: memory limit for the agent's short-lived tasks.

## CI

`.github/workflows/ci.yml` runs `bin/dev lint`, `bin/dev test` and `bin/dev package` on every
push and pull request and keeps `dist/` as an artifact. The live tests need `/dev/kvm` and
stay manual.

## Releases

A tag `v<major>.<minor>.<patch>` starts `.github/workflows/release.yml`:

1. `bin/dev all` (lint, test, package); the version comes from the tag.
2. CLI archives for Linux, macOS (`.tar.gz`) and Windows (`.zip`).
3. `packaging/apt/publish.sh` adds the Debian packages to the public APT repository
   `webcodingcat/clowder-apt` with reprepro and signs it; GitHub Pages serves it at
   `https://webcodingcat.github.io/clowder-apt` (distribution `trixie`, component `main`,
   public key `clowder.asc`). Its `README.md` shows how a host adds it.
4. A GitHub release with the packages and the archives.

The workflow needs two secrets in this repository:

| Secret | What |
|---|---|
| `APT_SIGNING_KEY` | ASCII-armoured private OpenPGP key without passphrase that signs the APT repository |
| `APT_DEPLOY_KEY` | private SSH key; its public half is a deploy key with write access on `webcodingcat/clowder-apt` |

`publish.sh` imports the key into a throw-away keyring and publishes the public half as
`clowder.asc`; `conf/` and `db/` stay in the APT repository so the next release adds to it.
