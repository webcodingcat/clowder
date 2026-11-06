# A cluster on Hetzner Cloud

This OpenTofu module creates a clowder cluster: the first host (`name`), the hosts that
join it (`hosts`), and the Floating IP. Each host is a Debian 13 server, and each worker
gets one volume per storage class. All hosts share one private network behind one Cloud
firewall. Cloud-init installs DRBD 9 from LINBIT and `clowderd` from clowder's APT
repository, and checks the host with `clowderd check`. Then it runs `clowderd init` on the
first host and `clowderd join` on the others. The main
[README](../../README.md#getting-started-on-hetzner-cloud) shows how to use the module.

| Setting | Default | What it does |
|---|---|---|
| `ssh_public_key` | required | the key that root accepts on each host |
| `admin_cidrs` | required | who reaches SSH (22) and clowder's API (7443); `0.0.0.0/0` and `::/0` are refused |
| `name` | `clowder0` | the first host's name, and the prefix of the resources at Hetzner |
| `cluster_name` | `clowder-cluster` | the cluster's name, and the context name of its credentials |
| `hosts` | `[]` | the names of the hosts that join the first one |
| `join_tokens` | `{}` | a join token for each host in `hosts` that has not joined yet |
| `roles` | `{}` | the roles of a host, by name; a host not named has all three |
| `published_ports` | `[]` | ports your stacks publish besides 80 and 443, open to everyone: `["udp/9987"]` |
| `storage_classes` | `{ fast = 20, bulk = 20 }` | one volume per class (size in GB) with the thin pool `vg-<name>/thin` |
| `location` | `nbg1` | `fsn1`, `nbg1` or `hel1` |
| `server_type` | `cx23` | the Hetzner server type |
| `network_ip_range` | `10.0.0.0/16` | the private network, in `10.0.0.0/8` but outside `10.77.0.0/16` and `10.78.0.0/16` |
| `clowderd_version` | this module's release | the pinned `clowderd` version, so later hosts match the cluster; empty means newest |
| `image` | `debian-13` | the hosts' image, or a prepared snapshot (below) |
| `clowderd_package_url`, `clowderd_package_sha256` | empty | test hook: install this `.deb` instead of using the APT repository |

The output `cluster_configuration` holds the ClusterConfiguration's `privateNetwork`,
`ingress.floatingIP` and `storageClasses`.

- **One Hetzner project per cluster.** Fencing powers servers off and on, and the Hetzner
  token (the Secret named by `hetzner.tokenSecretRef`) can do that to any server of its
  project. So keep other servers out of that project. A joining host names its own server.
  The leader checks with Hetzner that the server's private and public addresses are the
  host's, and never accepts the same server twice.
- **The Cloud firewall is the only filter** in front of SSH and the API. Only
  control-plane hosts get the API rule (a second firewall). Other hosts serve the API only
  to the control plane, over the private network. HTTP and HTTPS are open to everyone, for
  Traefik and the ACME CA's checks.
- **The first credentials** come over SSH from root on the first host, like kubeadm's
  `admin.conf`: `ssh root@<public_address> clowderd credentials | clowder config import -`.
  You trust the cluster's CA because you trust SSH, so the first host's user data holds
  nothing secret.
- **A joining host's user data holds its join token.** Root can read it in cloud-init's
  cache, and so can any process that reaches the metadata service. Once `clowderd` runs,
  its rules close that service to the host and its containers. The setup reads the service
  once, keeps what it needs in `/var/lib/clowder-setup`, and deletes its own copy of the
  token in `/etc/clowder`.
- **To add a host**, put a token from `clowder host join-token` into `join_tokens` under the
  host's name, and the name into `hosts`. A token works once and expires after an hour by
  default (`--ttl`, up to a day), so a host created later needs a new one. Once the host
  has joined, you can remove its token. Joins need the applied ClusterConfiguration,
  because the leader checks the server with its Hetzner token. A host that stays `joining`
  in `clowder host list` never got the cluster's answer: remove it with
  `clowder host remove`, put a new token into its `/etc/clowder/join-token`, and reboot it.
- **Roles.** A host has `control-plane`, `worker` and `ingress` unless `roles` says
  otherwise, such as `{ node3 = "worker,ingress" }`. The first host needs `control-plane`,
  and a host without `worker` gets no volumes. The token must allow the host's roles. A
  token from `clowder host join-token` allows only `worker,ingress` by default, so a host
  with the module's default roles needs `--roles control-plane,worker,ingress`. This is on
  purpose: the module's default keeps three hosts highly available, and the token's
  default stops a worker's token from creating a control-plane host. With default roles,
  the second host runs stacks but does not vote; once the third joins, all three vote.
  Roles are fixed once a host joins: to change them, remove the host and join it again.
- **To replace a host**, add the new one to `hosts` and wait until it is active. Then run
  `clowder host remove` for the old one and take it out of `hosts`. A cluster with three
  control-plane hosts keeps three. Every host joins through the first host, so the module
  cannot replace the first host yet, and no host can join while it is down.
- **`clowderd_version`** defaults to the release that this module belongs to, so check out
  the module at that release's tag.
- **Changes to a server's user data are ignored** after it is created. A storage class
  added later gets a volume, but no thin pool, on an existing host.
- **A prepared `image`** is a snapshot of a server where `setup.sh` ran with
  `PREPARE_ONLY=yes`: it holds the kernel headers, the DRBD module, Docker and the other
  dependencies of `clowderd`. On it, the setup skips what is there and only installs
  `clowderd`. clowder's live tests start their hosts this way. A changed `image` applies
  only to hosts created later and replaces no server.
