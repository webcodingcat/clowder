# Shipping

Part of [DEVELOPING.md](../../DEVELOPING.md): the Debian packages, CI and releases.

## Packages and units

`packaging/` holds the nfpm configurations, the systemd units and the maintainer scripts
(reload systemd after install and removal, restart a running agent after an upgrade, stop
the agent before removal). Packages that later phases need (conntrack, restic, git) come
with them.

- `clowderd` depends on Debian packages and on `drbd-dkms` and `drbd-utils` from LINBIT's
  public repository; the host's APT sources must list it. `coccinelle` lets the DKMS build
  run offline.
- `clowderd.service`: `Type=notify` with a watchdog, `Restart=always` without a start limit,
  `KillMode=process` (helpers finish their step; containers belong to dockerd). The package
  installs it without enabling it; `clowderd init` enables and starts it.
- An agent that stopped at a Raft entry or snapshot it cannot read exits with status 3
  ([Versions](cluster.md)), and `RestartPreventExitStatus=3` keeps systemd from starting
  it again: it would stop at the same entry, and a Raft member that comes and goes every
  few seconds would hide from the other hosts that it failed. The unit stays `failed`,
  with the reason in the journal; nothing reboots the host. The same holds for status 4,
  the agent of a server `clowderd recover` retired: it must never start again.
- An upgrade restarts a running agent (`postinstall.sh`, `systemctl try-restart`), and
  starts one that failed, so an agent stopped at an entry it could not read goes on with
  the version that reads it. A helper of the old agent (`docker compose up`, `drbdadm`) may
  still run then, and the new agent's first pass may start the same step for the same
  stack beside it; nothing guards against that yet. So upgrade in a maintenance window,
  while no stack changes.

## Updating a cluster

- Update the followers first, one after another, and the leader last (`clowder cluster
  status` names it). Go on with the next host only once the updated one is back: `clowder
  host list` shows its version.
- Before `v1.0.0` only agents of the same minor version work together, so update host by
  host only to a patch release of the same minor version (`0.2.0` to `0.2.3`). For a new
  minor version, stop the agent on every host (`systemctl stop clowderd`; the containers
  keep running), install the new package on every host (an upgrade starts no stopped
  agent), then start the agents again and update the CLI.
- The order keeps the cluster working, the gate keeps it consistent: the leader writes a
  new kind of command only once every agent reads it, and an agent that meets an entry it
  cannot read stops instead of leaving it out ([Versions](cluster.md)). Before `v1.0.0`
  that catches new kinds only, not a changed meaning of a field, so it does not make
  mixing minor versions safe.

## CI

`.github/workflows/ci.yml` runs `bin/dev lint`, `bin/dev test`, `bin/dev test-tools` and
`bin/dev package` on every push and pull request and keeps `dist/` as an artifact. `test-tools` checks the renderers' golden files with Debian's `nft -c`,
`drbdadm` and `docker-compose` (`test/tools/check-golden-files.sh`). The live tests need
`/dev/kvm` and stay manual.

## Releases

A tag `v<major>.<minor>.<patch>` starts `.github/workflows/release.yml`:

1. `bin/dev all` (lint, test, test-tools, package); the version comes from the tag.
2. CLI archives for Linux, macOS (`.tar.gz`) and Windows (`.zip`).
3. `packaging/apt/publish.sh` adds the Debian packages to the public APT repository
   `webcodingcat/clowder-apt` with `apt-ftparchive` and signs it; GitHub Pages serves it at
   `https://webcodingcat.github.io/clowder-apt` (distribution `trixie`, component `main`,
   public key `clowder.asc`). Its `README.md` shows how a host adds it.
4. A GitHub release with the packages and the archives. It comes last: a version is out once
   it is in the APT repository. If only this step fails, create the release by hand
   (`gh release create`); re-running the workflow fails, as `publish.sh` publishes a
   version once.

The workflow needs two secrets in this repository:

| Secret | What |
|---|---|
| `APT_SIGNING_KEY` | ASCII-armored private OpenPGP key without passphrase that signs the APT repository |
| `APT_DEPLOY_KEY` | private SSH key; its public half is a deploy key with write access on `webcodingcat/clowder-apt` |

`publish.sh` imports the key into a throw-away keyring and publishes the public half as
`clowder.asc`. The packages stay in the APT repository's pool, and the index lists every
version there, so a host pinned to an older release still installs it (Debian's reprepro
5.3 in Debian 13 has no `Limit` and keeps one version per package, so it is not used). A release candidate (`v1.2.3-rc1`,
Debian version `1.2.3~rc1`) lands in the same distribution; only a host without a pin
would take it. The APT repository still holds reprepro's `conf/` and `db/` from `v0.0.1`;
nothing reads them.

### Release checklist

The live tests on `main` install a package from `dist/` through the module's test hook. The
release path (APT repository, pin, CLI archive) is tested only on a release candidate:

1. `bin/dev live` on `main` is green.
2. `clowderd_version` in `examples/hetzner/variables.tf` names the new version, so the
   module at the tag installs its own release.
3. Tag `v1.2.3-rc1` and push it; the workflow publishes the candidate.
4. On a checkout of the tag, `bin/dev live release v1.2.3-rc1`: the host installs
   `1.2.3~rc1` from the APT repository, the tests use the CLI from the release's archive.
5. If it is green, tag `v1.2.3` on the same commit.

The candidate's version differs from the module's pin, which names the final one;
`bin/dev live release` pins the candidate itself. The release workflow takes the version
from the tag it runs on, so both tags may share the commit.
