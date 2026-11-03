# clowder

clowder runs stacks of containers with Docker Compose on a few small hosts (about 4 GB of
RAM). It replicates volumes with DRBD 9, keeps the client's real IP behind a Floating IP,
places services by priority and fences failed hosts through the hoster's API. The first
hoster is Hetzner Cloud.

**Status:** under construction; hosts join and stacks move between them, but no host
fails over yet. [docs/roadmap.md](docs/roadmap.md) says how far it is. Until
`v1.0.0`, a new release may need a cluster set up anew.

## What it does

- **Resources as with kubectl:** namespaces, stacks, volumes and secrets, with
  `clowder apply -f`, `get` (also `--watch`) and `delete`.
- **Secrets in your files, encrypted:** only the cluster can open them.
- **Placement by priority:** every service reserves memory and CPUs. A new stack goes
  where its data is; on full hosts, stacks of lower priority move to make room, or a new
  stack pushes out services of lower priority, which come back when there is room.
- **Replicated volumes:** with `replicas: 2` or `3`, DRBD keeps the copies on other
  hosts in sync, a volume with two copies gets a tiebreaker on a third host, and new
  copies resync one at a time at `replication.resync_rate`. A stack moves to another host
  with its data (`clowder stack move`, `clowder host drain`), its images pulled there
  first.
- **HTTP routes with certificates:** Traefik on the Floating IP, the client's address in
  `X-Forwarded-For`. A certificate comes from Let's Encrypt once the domain's DNS points at
  the Floating IP (not through a proxying CDN). The cluster accepts Let's Encrypt's terms
  of service for you when it makes its account.
- **The Floating IP stays on the ingress host,** also when someone moves it by hand.
- **Alerts** to Discord, email or a webhook, with an optional heartbeat;
  **metrics** for Prometheus on every host, which only the cluster's hosts reach.

## Getting started on Hetzner Cloud

You need two things on your machine:

- **The CLI `clowder`:** the archive for Linux, macOS or Windows from a
  [release](https://github.com/webcodingcat/clowder/releases), or on Debian and Ubuntu
  (amd64) the package `clowder` from clowder's APT repository
  (`https://webcodingcat.github.io/clowder-apt`, distribution `trixie`, component `main`).
- **The module [`examples/hetzner`](examples/hetzner)** for the first host, checked out at
  the same release, and [OpenTofu](https://opentofu.org). The module installs and checks
  everything and starts the agent; this takes about 5 minutes
  (`journalctl -u clowder-setup` shows how far it is).

You work in two directories: the module's, which holds the OpenTofu state, and the
**cluster directory** of your own, which `cluster bootstrap` makes and which you can share
in a repository. Keep them apart: the state holds the bootstrap key in clear. Commands in
the cluster directory find it themselves; elsewhere name it with `--cluster <directory>` or
`CLOWDER_CLUSTER`.

```bash
git clone --branch v0.1.0 https://github.com/webcodingcat/clowder.git
cd clowder/examples/hetzner
export HCLOUD_TOKEN=...
tofu init
tofu apply -var 'ssh_public_key=ssh-ed25519 AAAA...' -var 'admin_cidrs=["203.0.113.7/32"]'
mkdir ~/prod && cd ~/prod      # the cluster directory
clowder cluster bootstrap me --name prod \
  --seed "$(tofu -chdir=<module directory> output -raw public_address)" \
  --key-file <(tofu -chdir=<module directory> output -raw bootstrap_key)
```

`<(...)` hands the bootstrap key over without a file. Without Bash, write it to a file
outside the cluster directory and delete it after the bootstrap. Until the bootstrap, the
key lets anyone in who reaches the API; after it, it opens nothing.

The bootstrap shows the cluster's **recovery key** once and asks you to type its last 8
characters (`--recovery-out <file>` writes it to a file instead). Keep it in your password
manager and offline. If the cluster is lost, the recovery key still opens the values in
your files (`secret decrypt`) and moves them to a new cluster (`secret rekey --identity`).

A cluster has **one user**, the one the bootstrap makes. Do not share its credentials;
more users come later.

Lost your credentials, or your laptop? Get a code as root on a host (the Hetzner console
works without SSH): `clowderd user recovery-code me`. Then
`clowder user recover me --code <code>` gives you new credentials (outside the cluster
directory, add `--name prod --seed <address>`) and locks every other certificate of yours
out. With your credentials and the second code, `clowder recovery rotate --code <code>`
replaces the recovery key; then encrypt your files for it with
`clowder secret rekey -f <directory>`. Each code is valid for 15 to 30 minutes and works
once.

The bootstrap writes `cluster.yaml` into the cluster directory. Fill it in from
`tofu output cluster_yaml` in the module's directory, add the hoster's token, then run
`clowder cluster apply`:

```yaml
hoster:
  type: hetzner
  token: !secret |                 # clowder secret encrypt --cluster-yaml < token
    -----BEGIN AGE ENCRYPTED FILE-----
    ...
    -----END AGE ENCRYPTED FILE-----
```

`clowder cluster diff` shows what an apply would change. You can share the cluster
directory in a repository: your own key and certificate are in your config directory
(`~/.config/clowder/clusters/prod/` on Linux, `~/Library/Application Support/clowder/` on
macOS, `%AppData%\clowder\` on Windows). The module's settings, such as the firewall, are
in [its README](examples/hetzner/README.md).

**More hosts** join with a token each, once `cluster.yaml` is applied:
`clowder host join-token` prints one, which opens once and for an hour (`--ttl` up to a
day). Hand it to the module with the host's name:

```bash
tofu apply ... -var 'hosts=["clowder1", "clowder2"]' \
  -var 'join_tokens={clowder1 = "<token>", clowder2 = "<token>"}'
```

The second host waits as `passive`; with the third, all three run stacks and vote, and the
cluster stays at three hosts or more: `clowder host remove` refuses to go below, so a
replacement joins first, even for a host that is dead. `clowder host list` shows the
hosts. The leader checks with the
hoster that each host's server is its own, so give each cluster a Hetzner project of its
own. `clowder host drain` moves a host's stacks to the others for maintenance (its volume
copies stay), and `clowder host remove` of a host that still has stacks or copies moves
them off before it goes. A volume with `replicas: 0` holds throw-away data: it starts empty on
the new host whenever its stack moves. No host fails over yet.

## Running a stack

```yaml
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 10G, replicas: 1}   # replicas 0: one plain LV without DRBD, empty again after a move
---
apiVersion: clowder/v1
kind: Stack
metadata: {namespace: shop, name: web}
spec:
  priority: high
  services:
    db:
      image: postgres:17
      env: {POSTGRES_USER: shop}
      secretEnv: {POSTGRES_PASSWORD: db-password}
      volumes: [{volume: data, path: /var/lib/postgresql/data}]
      resources: {reserve: {memory: 512M, cpus: 0.5}}
      healthcheck: {command: [pg_isready], interval: 10s}
    app:
      image: registry.example.com/shop:1.4
      dependsOn: [db]
      secrets: [db-password]        # the file /run/secrets/db-password
      http: {host: shop.example.com, port: 8080}
```

`clowder secret encrypt shop/db-password` asks for the password and writes the Secret:

```yaml
apiVersion: clowder/v1
kind: Secret
metadata: {namespace: shop, name: db-password}
spec:
  value: !secret |
    -----BEGIN AGE ENCRYPTED FILE-----
    ...
    -----END AGE ENCRYPTED FILE-----
```

Put both in a directory and run `clowder apply -f shop/`. Then:

| Command | What it does |
|---|---|
| `clowder get stacks -n shop` | the stacks and where they run (`-A`: all namespaces) |
| `clowder stack ps\|logs shop/web` | the containers and their logs |
| `clowder stack restart shop/web [app]` | recreates services with their images pulled again |
| `clowder stack move shop/web node2` | moves the stack with its volumes' data to another host; a volume of replicas 0 starts there empty |
| `clowder delete -f shop/ --with-data` | deletes the resources; a volume only with `--with-data` |
| `clowder cluster status` | the hosts, the leader and the open alerts |
| `clowder secret rekey -f shop/` | encrypts the values in files again, for the cluster's current keys |
| `clowder secret decrypt --identity <key> -f <file>` | in an emergency, reads the values with the recovery key |

Good to know:

- Applying the same files again changes nothing.
- A field clowder does not know is an error.
- A secret's value is bound to its Secret: the cluster refuses it under another name.
  clowder refuses a secret in clear.
- Until clowder has permissions, every user can read every secret, through a stack.
- Names are lowercase letters, digits and `-`, at most 63 characters.
- Backups come with phase 3. Until then a volume's data is only on its hosts' disks,
  and `cluster.yaml` refuses `backup:`.

## Compatibility

| Binary | What it is | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) |

Before `v1.0.0`, CLI and agents work together only within one minor version: update all
hosts and the CLI together to a new minor version, host by host only to a patch release
(the followers first, the leader last). How the code works and how to build it:
[DEVELOPING.md](DEVELOPING.md).
