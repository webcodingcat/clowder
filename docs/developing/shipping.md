# Shipping

Part of [DEVELOPING.md](../../DEVELOPING.md): the Debian packages, CI and releases.

## Packages and units

`packaging/` holds the nfpm configurations, the systemd units and the maintainer scripts.
The scripts reload systemd after install and removal, restart a running agent after an
upgrade, and stop the agent before removal. Packages that later phases need (conntrack,
restic, git) are added with those phases.

- `clowderd` depends on Debian packages and on `drbd-dkms` and `drbd-utils` from LINBIT's
  public repository, so the host's APT sources must list that repository. `coccinelle` lets
  the DKMS build run offline.
- `clowderd.service` is `Type=notify` with a watchdog, `Restart=always` without a start
  limit, and `KillMode=process`: helpers finish their step, and containers belong to
  dockerd. The package installs the unit without enabling it; `clowderd init` enables and
  starts it.
- An agent that stopped at a Raft entry or snapshot it cannot read exits with status 3
  ([Versions](state.md#versions)). `RestartPreventExitStatus=3 4` keeps systemd from starting it
  again. A restarted agent would stop at the same entry, and a Raft member that comes and
  goes every few seconds would hide its failure from the other hosts. The unit stays
  `failed`, with the reason in the journal; nothing reboots the host. The same holds for
  status 4: the agent of a server that `clowderd recover` retired must never start again.
- A worker that `clowderd recover` kept ends its agent with status 5, to start again as a
  fresh Raft member (Recover in [cluster.md](cluster.md)). `SuccessExitStatus=5` counts
  that as a success, so the unit shows no failure, and `Restart=always` starts it again.
- An upgrade restarts a running agent (`postinstall.sh`, `systemctl try-restart`) and also
  starts one that failed. So an agent that stopped at an entry it could not read continues
  with the version that can read it. A helper of the old agent (`docker compose up`,
  `drbdadm`) may still be running at that point, and the new agent's first pass may start
  the same step for the same stack next to it. Nothing guards against that yet, so upgrade
  in a maintenance window, while no stack changes.

## Updating a cluster

[Updating a cluster](../operating.md#updating-a-cluster) gives the steps for users. The
order keeps the cluster working, and the version gate keeps it consistent. The leader
writes a new kind of command only once every agent can read it, and an agent that meets an
entry it cannot read stops instead of skipping it ([Versions](state.md#versions)). Before
`v1.0.0` this catches only new kinds of commands, not a changed meaning of a field, so it
does not make mixing minor versions safe.

## CI

`.github/workflows/ci.yml` runs `bin/dev lint`, `bin/dev test`, `bin/dev test-tools` and
`bin/dev package` on every push and pull request, and keeps `dist/` as an artifact.
`test-tools` checks the renderers' golden files with Debian's `nft -c`, `drbdadm` and
`docker-compose` (`test/tools/check-golden-files.sh`). The live tests need `/dev/kvm` and
stay manual.

## Releases

A tag `v<major>.<minor>.<patch>` starts `.github/workflows/release.yml`:

1. `bin/dev all` (lint, test, test-tools, package); the version comes from the tag.
2. CLI archives for Linux, macOS (`.tar.gz`) and Windows (`.zip`).
3. `packaging/apt/publish.sh` adds the Debian packages to the public APT repository
   `webcodingcat/clowder-apt` with `apt-ftparchive` and signs it. GitHub Pages serves it at
   `https://webcodingcat.github.io/clowder-apt` (distribution `trixie`, component `main`,
   public key `clowder.asc`). Its `README.md` shows how a host adds it.
4. A GitHub release with the packages and the archives. This step comes last, because a
   version counts as released once it is in the APT repository. If only this step fails,
   create the release by hand (`gh release create`). Re-running the workflow fails,
   because `publish.sh` publishes a version only once.

The workflow needs two secrets in this repository:

| Secret | What |
|---|---|
| `APT_SIGNING_KEY` | ASCII-armored private OpenPGP key without passphrase that signs the APT repository |
| `APT_DEPLOY_KEY` | private SSH key; its public half is a deploy key with write access to `webcodingcat/clowder-apt` |

`publish.sh` imports the key into a throw-away keyring and publishes the public half as
`clowder.asc`. The packages stay in the APT repository's pool, and the index lists every
version there, so a host pinned to an older release can still install it. reprepro is not
used: Debian 13 ships reprepro 5.3, which has no `Limit` and keeps only one version per
package. A release candidate (`v1.2.3-rc1`, Debian version `1.2.3~rc1`) lands in the same
distribution; only a host without a pin would install it. The APT repository still holds
reprepro's `conf/` and `db/` from `v0.0.1`; nothing reads them.

### Release checklist

The live tests on `main` install a package from `dist/` through the module's test hook. The
release path (APT repository, pin, CLI archive) is tested only on a release candidate:

1. `bin/dev live` on `main` is green.
2. `clowderd_version` in `examples/hetzner/variables.tf` names the new version, so the
   module at the tag installs its own release.
3. Tag `v1.2.3-rc1` and push it; the workflow publishes the candidate.
4. On a checkout of the tag, run `bin/dev live release v1.2.3-rc1`. The hosts' image is
   built from scratch with the release's setup and dependencies, the hosts install
   `1.2.3~rc1` from the APT repository, and the tests use the CLI from the release's
   archive.
5. If it is green, tag `v1.2.3` on the same commit.

The candidate's version differs from the module's pin, which names the final version, so
`bin/dev live release` pins the candidate itself. The release workflow takes the version
from the tag it runs on, so both tags can point to the same commit.
