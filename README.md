# clowder

clowder runs stacks of containers with Docker Compose on a few small hosts (about 4 GB of
RAM). It replicates volumes with DRBD 9, keeps the client's real IP behind a Floating IP,
places services by priority and fences failed hosts through Hetzner Cloud's API. It runs
on Hetzner Cloud, with a Hetzner private network between the hosts.

Until `v1.0.0`, a new release may need a cluster set up anew.

## What it does

- **Resources as with kubectl:** the cluster's configuration, namespaces, stacks, volumes
  and secrets, with `clowder apply -f` (also `--dry-run`), `diff -f`, `get` (also
  `--watch`) and `delete`. An apply goes in whole or not at all.
- **Secrets as in Kubernetes with sealed secrets:** a `Secret` in clear, or a
  `SealedSecret` that only the cluster can open and that can go into Git. A stack gets
  them as files or variables; registry logins belong to a namespace.
- **Placement by priority:** every service reserves memory and CPUs. A new stack goes
  where its data is. On full hosts, stacks of lower priority move to make room, or a new
  stack pushes out services of lower priority, which come back when there is room.
- **Replicated volumes:** with `replicas: 2` or `3`, DRBD keeps copies on other hosts in
  sync, and a volume with two copies gets a tiebreaker on a third host. A stack moves to
  another host with its data (`clowder stack move`, `clowder host drain`).
- **Failover with fencing:** when the other hosts stop hearing a host with stacks for
  45 s, clowder powers it off through Hetzner's API and starts its stacks on a host with a
  synced copy of their data, within about three minutes.
- **Host roles as in Kubernetes:** `control-plane`, `worker` and `ingress`. Only
  control-plane hosts hold the cluster's keys. A worker gets only the secrets of its own
  stacks, as Kubernetes' node authorizer allows, and serves only the control plane, over
  the private network.
- **HTTP routes with certificates:** Traefik on the Floating IP, with the client's address
  in `X-Forwarded-For`, and certificates from Let's Encrypt once the domain's DNS points
  at the Floating IP (not through a proxying CDN). The cluster accepts Let's Encrypt's
  terms of service for you when it creates its account. The Floating IP moves when the
  ingress host drains or fails, and every host routes to every stack.
- **Alerts** to Discord, email or a webhook, with an optional heartbeat, and **metrics**
  for Prometheus on every host, which only the cluster's hosts can reach.

clowder makes no backups. [docs/operating.md](docs/operating.md#known-limits) lists
what else it does not do.

## Getting started on Hetzner Cloud

You need:

- the CLI `clowder`: the archive for Linux, macOS or Windows from a
  [release](https://github.com/webcodingcat/clowder/releases), or on Debian and Ubuntu
  (amd64) the package `clowder` from the APT repository
  `https://webcodingcat.github.io/clowder-apt`, distribution `trixie`, component `main`;
- [OpenTofu](https://opentofu.org);
- the module [`examples/hetzner`](examples/hetzner) of the same release as your CLI.

The module sets up the first host in about 5 minutes (`journalctl -u clowder-setup` shows
the progress); [its README](examples/hetzner/README.md) explains its settings. Replace
`<release>` with your CLI's version (`clowder version`), such as `v0.2.0`:

```bash
git clone --branch <release> https://github.com/webcodingcat/clowder.git
cd clowder/examples/hetzner
export HCLOUD_TOKEN=...
tofu init
tofu apply -var 'ssh_public_key=ssh-ed25519 AAAA...' -var 'admin_cidrs=["203.0.113.7/32"]'
ssh "root@$(tofu output -raw public_address)" clowderd credentials | clowder config import -
clowder cluster status
ssh "root@$(tofu output -raw public_address)" clowderd sealing-key export > sealing-key
```

- **Credentials** come over SSH from root on a host, as kubeadm's `admin.conf` does, so
  check the host's key on first contact. Whoever holds them controls the cluster.
- **The sealing key** opens every SealedSecret. Keep the file offline.

[docs/operating.md](docs/operating.md) explains both.

The cluster's settings are one resource, the **`ClusterConfiguration`** named `cluster`.
Its secrets are Secrets in the namespace `clowder-system`. `tofu output
cluster_configuration` gives the fields that come from the module. Seal the Hetzner
project's API token with `clowder secret seal clowder-system/hetzner --from-file token=-`
(it asks for the token), and put the SealedSecret above the configuration in one file:

```yaml
apiVersion: clowder/v1
kind: SealedSecret
metadata: {namespace: clowder-system, name: hetzner}
spec:
  encryptedData:
    token: YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSAu...
---
apiVersion: clowder/v1
kind: ClusterConfiguration
metadata: {name: cluster}
spec:
  hetzner:
    tokenSecretRef: {name: hetzner, key: token}
  privateNetwork: 10.0.0.0/16
  ingress: {floatingIP: 203.0.113.10}
  storageClasses:
    fast: {pool: vg-fast/thin}
    bulk: {pool: vg-bulk/thin}
```

Run `clowder apply -f system.yaml`. The file holds no value in clear, so it can live in a
repository. Keep it apart from the module's directory, whose OpenTofu state holds the join
tokens. An apply that brings a new Hetzner token first says how many of the hosts'
servers the token sees and asks (`--yes` skips the question).
[docs/resources.md](docs/resources.md) explains the configuration.

**More hosts** join with a token each. The module gives every host all three roles unless
its `roles` say otherwise, so the token needs all three:

```bash
clowder host join-token --roles control-plane,worker,ingress   # one per host
tofu apply ... -var 'hosts=["clowder1", "clowder2"]' \
  -var 'join_tokens={clowder1 = "<token>", clowder2 = "<token>"}'
```

With the third control-plane host, all three vote. Then fetch new credentials, so that the
CLI knows the new hosts. [Hosts and roles](docs/operating.md#hosts-and-roles) explains the
roles and what to do when a host fails.

## Running a stack

```yaml
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
spec:
  imagePullSecrets: [{name: registry}]   # a Secret with the keys server, username, password
---
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 10G, replicas: 1}   # replicas 0: a plain LV, empty again after a move
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
      secretEnv: {POSTGRES_PASSWORD: {name: db, key: password}}
      volumes: [{volume: data, path: /var/lib/postgresql/data}]
      resources: {reserve: {memory: 512M, cpus: 0.5}}
      healthcheck: {command: [pg_isready], interval: 10s}
    app:
      image: registry.example.com/shop:1.4
      dependsOn: [db]
      secrets: [db]                 # the file /run/secrets/db/password
      http: {host: shop.example.com, port: 8080}
```

`clowder secret seal shop/db --from-file password=-` asks for the password and prints the
SealedSecret `db` for your files. `--from-literal <key>=<value>` and `--from-file
<key>=<path>` add more keys, and `secret seal -f <file>` seals the Secrets of a file. The
registry login is a Secret too: `clowder secret seal shop/registry --from-literal
server=registry.example.com --from-literal username=shop --from-file password=-`. A Secret
that stays out of Git can go in clear instead: in a file (`spec.data` in Base64,
`spec.stringData` as text), or with `clowder create secret shop/db --from-file
password=-`.

Put the files in a directory and run `clowder apply -f shop/`. clowder reads the files in
the order of their names, and each file from top to bottom. A resource must come after the
resources it needs: the Namespace first, the Secrets before the Stack. Applying the same
files again changes nothing, and a field that clowder does not know is an error.
[docs/resources.md](docs/resources.md) describes every field.

| Command | What it does |
|---|---|
| `clowder get stacks -n shop` | the stacks and where they run (`-A`: all namespaces) |
| `clowder stack ps\|logs shop/web` | the containers and their logs |
| `clowder stack restart shop/web [app]` | recreates services, with their images pulled again |
| `clowder stack move shop/web node2` | moves the stack with its volumes' data to another host |
| `clowder volume resolve-splitbrain shop/data --keep node1` | after the alert `split-brain`: the copy on node1 keeps its data, and the others take it over |
| `clowder diff -f shop/` | what an apply would change (exit code 1 when something would) |
| `clowder delete -f shop/ --with-data` | deletes the resources from the bottom up; a volume only with `--with-data` |
| `clowder cluster status` | the configuration's generation, the hosts, the leader and the open alerts |
| `clowder host list\|drain\|undrain\|remove` | the hosts with their roles; `drain` moves a host's stacks away for maintenance |
| `clowder secret rekey -f shop/ --identity sealing-key` | seals the values in files again, for the cluster of the context (`--cert`: for a public sealing key) |
| `clowder secret decrypt --identity sealing-key -f <file>` | in an emergency, reads the values of SealedSecrets with the sealing key |

## Compatibility

| Binary | What it is | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) on Hetzner Cloud servers in one project, with a Hetzner private network between them |

Before `v1.0.0`, the CLI and the agents work together only within one minor version
([Updating a cluster](docs/operating.md#updating-a-cluster)). How the code works and how
to build it: [DEVELOPING.md](DEVELOPING.md).
