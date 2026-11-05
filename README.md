# clowder

clowder runs stacks of containers with Docker Compose on a few small hosts (about 4 GB of
RAM). It replicates volumes with DRBD 9, keeps the client's real IP behind a Floating IP,
places services by priority and fences failed hosts through Hetzner Cloud's API. It runs
on Hetzner Cloud, with a Hetzner private network between the hosts.

**Status:** under construction. Three hosts and more work: hosts join, stacks move between
them with their data, and a failed host is fenced and its stacks start elsewhere. Backups
come next. [docs/roadmap.md](docs/roadmap.md) says how far it is. Until `v1.0.0`, a new
release may need a cluster set up anew.

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
- **Failover with fencing:** a host with stacks that the others stop hearing for 45 s is
  powered off through Hetzner's API, and its stacks start on a host with a synced copy of
  their data, within about three minutes. The host is powered on again and takes stacks
  once its copies caught up; one that fails three times within an hour stays off until
  `clowder host enable`.
- **HTTP routes with certificates:** Traefik on the Floating IP, the client's address in
  `X-Forwarded-For`. A certificate comes from Let's Encrypt once the domain's DNS points at
  the Floating IP (not through a proxying CDN). The cluster accepts Let's Encrypt's terms
  of service for you when it makes its account.
- **The Floating IP stays on the ingress host,** also when someone moves it by hand, and
  follows to another host when the ingress host drains or fails. Every host routes to
  every stack, so the stacks need not run where the Floating IP is.
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

```bash
git clone --branch v0.1.0 https://github.com/webcodingcat/clowder.git
cd clowder/examples/hetzner
export HCLOUD_TOKEN=...
tofu init
tofu apply -var 'ssh_public_key=ssh-ed25519 AAAA...' -var 'admin_cidrs=["203.0.113.7/32"]'
ssh "root@$(tofu output -raw public_address)" clowderd credentials | clowder config import -
clowder cluster status
clowder recovery rotate        # the cluster's first recovery key, shown once
```

The **first credentials** come over SSH from root on the first host, as kubeadm's
`admin.conf` does: `clowderd credentials` prints a CLI config with one context, named
after the cluster (`cluster_name` of the module, `clowder-cluster` by default), and
`clowder config import` adds it to yours. They hold the cluster's CA, so you trust the
cluster as far as you trust the SSH connection: check the host's key on first contact,
as for any new server (on the Hetzner console: `ssh-keygen -lf
/etc/ssh/ssh_host_ed25519_key.pub`).

The CLI keeps its credentials in `~/.clowder/config` (or `--config`, or
`$CLOWDER_CONFIG`), as kubectl keeps `~/.kube/config`: contexts, each with the cluster's
endpoints, its CA and your key and certificate. `clowder config get-contexts` lists them,
`clowder config use-context <name>` switches, `--context <name>` picks one for a single
command, and `clowder config delete-context <name>` removes one. A cluster has **no
users**, as with kubeadm's `admin.conf`: whoever holds a context controls the cluster.
`clowderd credentials --name <you>` puts your name into the certificate, for the audit and
the agents' logs (`cli/<you>`); it grants nothing.

A CLI certificate lasts a **year**; from 30 days before its end every command warns.
New credentials, or new ones after a lost laptop, come the same way from root on any host
(the Hetzner console works without SSH): `clowder config import - --replace` puts them in
place of the old ones, `--as <name>` adds them under another name.
`clowderd credentials --revoke-older` also locks
out every CLI certificate issued before the new one.

A new cluster has no **recovery key** yet, and nothing can be encrypted for it until it
has one: `clowder recovery rotate` makes it, shows it
once and asks you to type its last 8 characters (`--recovery-out <file>` writes it to a
file instead). Keep it in your password manager and offline. If the cluster is lost, the
recovery key still opens the values in your files (`secret decrypt`) and moves them to a
new cluster (`secret rekey --identity`). `clowder recovery rotate` replaces it later too;
then encrypt your files for the new one with `clowder secret rekey -f <directory>`.

`cluster.yaml` lives in a **cluster directory** of your own, which you can share in a
repository: no key is in it. Commands in the cluster directory find it themselves;
elsewhere name it with `--cluster <directory>` or `CLOWDER_CLUSTER`. Keep it apart from the
module's directory, whose OpenTofu state holds the join tokens.

`hetzner.api` and `acme.directory` in `cluster.yaml` are for test labs: unset, clowder
talks to Hetzner's API and Let's Encrypt over https. Set, they may be `http://`, and
`cluster apply` and the leader's log warn about it.

Write `cluster.yaml` from `tofu output cluster_yaml` in the module's directory (it brings
`private_network`, the Floating IP and the storage classes; `clowder cluster schema`
describes every field), add the Hetzner project's API token, then run
`clowder cluster apply`:

```yaml
hetzner:
  token: !secret |                 # clowder secret encrypt --cluster-yaml < token
    -----BEGIN AGE ENCRYPTED FILE-----
    ...
    -----END AGE ENCRYPTED FILE-----
```

`clowder cluster diff` shows what an apply would change. The module's settings, such as
the firewall, are in [its README](examples/hetzner/README.md).

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
hosts.

The CLI reaches the cluster only through the `endpoints` of its context, in turn, and
learns no others. `clowderd credentials` lists every host the cluster has at the time (or
what `--endpoint` names, such as a load balancer in front of them): once hosts have joined,
fetch new credentials or add their public addresses (`clowder host list`) to the context
in `~/.clowder/config`; with the first host alone, the CLI cannot reach the cluster while
that host is down. `--connect-timeout` (default 5s) is how long it waits for an endpoint
before it tries the next.

The leader checks with
Hetzner that each host's server is its own, so give each cluster a Hetzner project of its
own. `clowder host drain` moves a host's stacks to the others for maintenance (its volume
copies stay), and `clowder host remove` of a host that still has stacks or copies moves
them off before it goes. A volume with `replicas: 0` holds throw-away data: it starts empty on
the new host whenever its stack moves or fails over.

A host that fails is powered off and its stacks start elsewhere: a volume with
`replicas: 2` or `3` keeps its data, one with `replicas: 1` waits for its host. When
Hetzner's API cannot power the host off, nothing fails over and an alert says so; once you
know the server is off, `clowder host fence-confirm <host>` lets the stacks start
elsewhere.

When two of three hosts are gone for good, the cluster has no majority and decides
nothing. As root on the host that is left, `clowderd recover --power-off` powers the
others off through Hetzner's API and labels their servers `clowder-retired`, so their agents
never start again, then goes on with this host alone. It runs the stacks with the data
of its copies; a volume with no synced copy there waits for a restore. Join two new
hosts as above, and set the retired servers up anew or delete them.

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
| `clowder volume resolve-splitbrain shop/data --keep node1` | after the alert `split-brain`: the copy on node1 keeps its data, the others take it |
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
| `clowderd` | the agent on each host | Debian 13 (amd64) on Hetzner Cloud servers in one project, with a Hetzner private network between them |

Before `v1.0.0`, CLI and agents work together only within one minor version: update all
hosts and the CLI together to a new minor version, host by host only to a patch release
(the followers first, the leader last). How the code works and how to build it:
[DEVELOPING.md](DEVELOPING.md).
