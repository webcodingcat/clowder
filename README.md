# clowder

A small cluster orchestrator that runs stacks of containers with Docker Compose on hosts
with about 4 GB of RAM: replicated volumes (DRBD 9), the real client IP behind a Floating
IP, priorities, fencing through the hoster's API. First hoster: Hetzner Cloud.

Status: under construction, on one host so far; [docs/roadmap.md](docs/roadmap.md) says
how far it is. Until `v1.0.0`, the state and `cluster.yaml` may change between releases
without a way to upgrade a running cluster. On one host, what runs today:

- `clowderd check` checks a host, `clowderd init` makes it the first of a cluster and starts
  the agent, which keeps the containers apart and answers their DNS.
- `clowder cluster bootstrap|apply|diff|status|schema` sets the cluster up and changes it.
- `clowder host ...` drains, removes and enables hosts.
- The cluster raises alerts (a pool or the root file system filling up, Docker not
  answering, the Floating IP not held, a host over capacity, a certificate not renewed, a
  stack waiting) and sends them to Discord, email or a webhook (`alerts` in
  `cluster.yaml`), with a reminder every 6 hours and an all-clear; an optional heartbeat
  lets an outside service notice when the cluster falls silent. `clowder cluster status`
  shows the open alerts, `clowder alert test` tries every channel.
- Every host serves `/metrics` for Prometheus on its node address, port 7444: the host's
  phase, pools, containers and restarts, and from the leader the room of each host, the
  certificates' expiry and the open alerts. Only the cluster's hosts reach it.
- `clowder apply -f <file|directory|->`, `clowder get <kind>` and `clowder delete <kind>
  <name>` manage resources as kubectl does: namespaces, stacks, volumes and secrets. A
  namespace comes with `-n` or in the name (`shop/web`), else it is `default`; `-A` lists
  all. Applying the same files again changes nothing; `clowder get --watch` goes on to
  show each resource that changes. `clowder stack ps|logs
  [<namespace>/]<stack>` show a stack's containers and logs; `clowder stack restart
  [<namespace>/]<stack> [<service>...]` recreates services (all, or those named, with
  what depends on them) with their images pulled again.
- Every service reserves memory and CPUs (`resources.reserve`, else
  `defaults.reservations`), and a stack goes only where its reservations fit. When a host is
  full, a new stack's services push out services of lower priority (`priority`, a number or
  a category of `priorities`), with what depends on them; they come back once there is room
  again for a minute.
- A service with `http: {host, port}` gets requests for that host on the
  Floating IP through Traefik, only to containers that run and are healthy; the client's
  address arrives in `X-Forwarded-For`. The cluster gets a certificate for the host from
  Let's Encrypt (or the CA of `acme.directory`) once its DNS points at the Floating IP, then
  redirects HTTP to HTTPS and renews the certificate when a third of its lifetime is left
  (`acme.renew_before` to change that). A domain whose DNS points elsewhere, such as a
  proxying CDN, gets no certificate. The cluster accepts the CA's terms of service for you
  when it makes its account.
- The cluster keeps the Floating IP routed to its ingress host through the hoster's API
  (`hoster` in `cluster.yaml`), also when someone moves it by hand.

A stack with a volume and a password:

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

The secret goes in a SOPS file of its own, `db-password.sops.yaml` (`kind: Secret`,
`spec: {value: ...}`), which `clowder apply` decrypts with your age key.

- A stack's spec knows only what clowder runs: a field it does not have is an error.
- A volume outlives its stack. Deleting a volume deletes its data, so `clowder delete`
  takes one only with `--with-data`.
- A secret in a file in clear is refused; in clear it comes only on stdin
  (`clowder apply -f -`). The cluster keeps secrets sealed.
- Who can read a secret: whoever may apply it or decrypt its file, every host of the
  cluster, and, until clowder has permissions, every user, through a stack that names it
  or one that `cluster.yaml` allows privileges (`stacks.privileged`, as
  `<namespace>/<stack>`).

Names of namespaces, stacks, services, volumes and secrets are lowercase letters, digits
and `-`, at most 63 characters; `_` is not allowed.

| Binary | For | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) |

## A host on Hetzner Cloud

[`examples/hetzner`](examples/hetzner) is an OpenTofu module for the first host: the server
(Debian 13), a private network, the Floating IP, one volume per storage class and a Cloud
firewall. Its cloud-init installs DRBD 9 from LINBIT and `clowderd` from clowder's APT
repository, prepares the host, checks it with `clowderd check` and runs `clowderd init`. This
takes about 5 minutes, more when it reboots the host into a newer kernel (at most twice);
`journalctl -u clowder-setup` shows how far it got.

```bash
export HCLOUD_TOKEN=...
tofu init
tofu apply -var 'ssh_public_key=ssh-ed25519 AAAA...' -var 'admin_cidrs=["203.0.113.7/32"]'
tofu output -raw bootstrap_key > bootstrap-key
clowder cluster bootstrap me --name prod --age age1... \
  --seed "$(tofu output -raw public_address)" --key-file bootstrap-key
```

Then fill in `cluster.yaml` from `tofu output cluster_yaml`, put the hoster token into
`secrets.sops.yaml` (`sops edit`) and run `clowder cluster apply`.

Your credentials (your key, your certificate and the cluster's CA) do not go into the
cluster directory, which you may share in a repository, but into your config directory,
under the cluster's `name`: `~/.config/clowder/clusters/prod/credentials.yaml` on Linux,
`~/Library/Application Support/clowder/...` on macOS, `%AppData%\clowder\...` on Windows.
Only you can read the file, as with SSH keys; keep it as safe.

- **The Cloud firewall is the only filter** in front of SSH and clowder's API (port 7443):
  `admin_cidrs` names who reaches them. It takes no `0.0.0.0/0`. HTTP and HTTPS are open to
  everyone (Traefik, and the ACME CA's checks); a port your stacks publish opens with
  `published_ports` (`["udp/9987"]`).
- **The bootstrap key** lets the first user in and later hosts join. It is in the OpenTofu
  state, encrypted in `secrets.sops.yaml`, and in the server's user data: root on the host
  can read it from cloud-init's cache and the metadata service, which containers cannot
  reach. That gives root nothing new, as root on a host holds the cluster's keys anyway; the
  setup deletes only its own copy in `/etc/clowder`. Keep the state as safe as
  `secrets.sops.yaml`.
- Pin `clowderd_version`, so a host made later gets the cluster's version.
- The module makes one host; more come with `clowderd join` (not there yet). It ignores later
  changes to the user data, so a storage class added to `storage_classes` gets a volume but
  no thin pool on the host.
- `clowderd_package_url` and `clowderd_package_sha256` are a test hook: they install a
  `.deb` instead of the APT repository.

Building and the layout of the code: [DEVELOPING.md](DEVELOPING.md).
