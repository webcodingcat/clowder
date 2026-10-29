# clowder

A small cluster orchestrator for Docker Compose stacks on hosts with about 4 GB of RAM:
replicated volumes (DRBD 9), the real client IP behind a Floating IP, priorities, fencing
through the hoster's API. First hoster: Hetzner Cloud.

Status: under construction (phase 1a, see the roadmap in [DEVELOPING.md](DEVELOPING.md)).
On one host, what runs today:

- `clowderd check` checks a host, `clowderd init` makes it the first of a cluster and starts
  the agent, which keeps the containers apart and answers their DNS.
- `clowder cluster bootstrap|apply|diff|status|schema` sets the cluster up and changes it.
- `clowder host ...` drains, removes and enables hosts.
- `clowder stack create|apply|delete|ps|logs` runs Compose stacks with their volumes and
  secrets.

A stack's passwords live in a SOPS file next to its `clowder.yaml`; the CLI decrypts it
with your age key, and the cluster keeps the values sealed. Who can read them: whoever can
decrypt that file, every host of the cluster, and, until clowder has permissions, every user
through a stack that `cluster.yaml` allows privileges (`stacks.privileged`).

| Binary | For | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) |

## A host on Hetzner Cloud

[`examples/hetzner`](examples/hetzner) is an OpenTofu module for the first host: the server
(Debian 13), a private network, the Floating IP, one volume per storage class and a Cloud
firewall. Its cloud-init installs DRBD 9 from LINBIT and `clowderd` from clowder's APT
repository, prepares the host, checks it with `clowderd check` and runs `clowderd init`. This
takes about 5 minutes, more when it reboots the host into a newer kernel (at most twice); `journalctl -u clowder-setup`
shows how far it got.

```bash
export HCLOUD_TOKEN=...
tofu init
tofu apply -var 'ssh_public_key=ssh-ed25519 AAAA...' -var 'admin_cidrs=["203.0.113.7/32"]'
tofu output -raw bootstrap_key > bootstrap-key
clowder cluster bootstrap me --age age1... --seed "$(tofu output -raw public_address)" \
  --key-file bootstrap-key
```

Then fill in `cluster.yaml` from `tofu output cluster_yaml`, put the hoster token into
`secrets.sops.yaml` (`sops edit`) and run `clowder cluster apply`.

- **The Cloud firewall is the only filter** in front of SSH and clowder's API (port 7443):
  `admin_cidrs` names who reaches them. It takes no `0.0.0.0/0`.
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
