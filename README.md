# clowder

clowder runs stacks of containers with Docker Compose on a few small hosts (about 4 GB of
RAM). It replicates volumes with DRBD 9, keeps the client's real IP behind a Floating IP,
places services by priority and fences failed hosts through the hoster's API. The first
hoster is Hetzner Cloud.

**Status:** under construction and on one host so far; [docs/roadmap.md](docs/roadmap.md)
says how far it is. Until `v1.0.0`, a new release may need a cluster set up anew.

## What it does

- **Resources as with kubectl:** namespaces, stacks, volumes and secrets, with
  `clowder apply -f`, `get` (also `--watch`) and `delete`.
- **Secrets in your files, encrypted:** only the cluster can open them.
- **Placement by priority:** every service reserves memory and CPUs. On a full host, a
  new stack pushes out services of lower priority, which come back when there is room.
- **HTTP routes with certificates:** Traefik on the Floating IP, the client's address in
  `X-Forwarded-For`. A certificate comes from Let's Encrypt once the domain's DNS points at
  the Floating IP (not through a proxying CDN). The cluster accepts Let's Encrypt's terms
  of service for you when it makes its account.
- **The Floating IP stays on the ingress host,** also when someone moves it by hand.
- **Alerts** to Discord, email or a webhook, with an optional heartbeat;
  **metrics** for Prometheus on every host, which only the cluster's hosts reach.

## Getting started on Hetzner Cloud

[`examples/hetzner`](examples/hetzner) is an OpenTofu module for the first host. It
installs and checks everything and starts the agent; this takes about 5 minutes
(`journalctl -u clowder-setup` shows how far it is).

```bash
export HCLOUD_TOKEN=...
tofu init
tofu apply -var 'ssh_public_key=ssh-ed25519 AAAA...' -var 'admin_cidrs=["203.0.113.7/32"]'
tofu output -raw bootstrap_key > bootstrap-key
clowder cluster bootstrap me --name prod --seed "$(tofu output -raw public_address)" \
  --key-file bootstrap-key
```

The bootstrap shows the cluster's **recovery key** once and asks you to type its last 8
characters (`--recovery-out <file>` writes it to a file instead). Keep it in your password
manager and offline. If the cluster is lost, the recovery key still opens the values in
your files (`secret decrypt`) and moves them to a new cluster (`secret rekey --identity`).

The bootstrap writes `cluster.yaml` into the current directory. Fill it in from
`tofu output cluster_yaml`, add the hoster's token, then run `clowder cluster apply`:

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

## Running a stack

```yaml
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 10G, replicas: 1}   # replicas 0: one plain LV without DRBD
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

## Compatibility

| Binary | What it is | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) |

CLI and agent work together across one minor version. How the code works and how to build
it: [DEVELOPING.md](DEVELOPING.md).
