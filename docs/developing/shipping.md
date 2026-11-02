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
- An upgrade restarts a running agent (`postinstall.sh`, `systemctl try-restart`). A helper
  of the old agent (`docker compose up`, `drbdadm`) may still run then, and the new agent's
  first pass may start the same step for the same stack beside it; nothing guards against
  that yet. So upgrade in a maintenance window, while no stack changes.

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
