# A cluster on Hetzner Cloud

An OpenTofu module for a clowder cluster. It creates the first host (`name`), the hosts
that join it (`hosts`), and the cluster's Floating IP. Each host is a server (Debian 13)
with one volume per storage class, and all hosts are in one private network behind one
Cloud firewall. The module's cloud-init installs DRBD 9 from LINBIT and `clowderd` from
clowder's APT repository, prepares the host, and checks it with `clowderd check`. It then
runs `clowderd init` on the first host and `clowderd join` on the others. The main
[README](../../README.md#getting-started-on-hetzner-cloud) explains how to use it.

- **One Hetzner project per cluster.** The Hetzner token (the Secret that the
  `ClusterConfiguration`'s `hetzner.tokenSecretRef` names) can power off and on any server
  of its project, and fencing does exactly that. Keep other servers out of the cluster's
  project. A joining host names its own server. The leader checks with Hetzner that the
  server's private and public addresses are the host's, and it never accepts the same
  server twice.
- **The Cloud firewall is the only filter** in front of SSH and clowder's API (port 7443).
  `admin_cidrs` names who can reach them; it does not accept `0.0.0.0/0`. HTTP and HTTPS
  are open to everyone (for Traefik and the ACME CA's checks). To open a port that your
  stacks publish, add it to `published_ports` (`["udp/9987"]`).
- **The first credentials** come over SSH from root on the first host, as kubeadm's
  `admin.conf` does: `ssh root@<public_address> clowderd credentials | clowder config
  import -`. You trust the cluster's CA because you trust SSH; the first host's user data
  contains nothing secret. `cluster_name` names the cluster (`clowderd init
  --cluster-name`), and with it the context of the credentials. `name` is the first host's
  name and the prefix of the module's resources at Hetzner, which the cluster never sees.
- **The user data** of a joining host contains its join token. Root can read it from
  cloud-init's cache, and the metadata service hands it to any process that reaches the
  service. Once `clowderd` runs, its rules close the service to the host and its
  containers. The setup reads the service once and keeps what it needs in
  `/var/lib/clowder-setup` (the private networks only once they list an address). It then
  deletes its own copy of the token in `/etc/clowder`.
- **More hosts** join with one token each. `clowder host join-token` prints a token; put it
  into `join_tokens` under the host's name, and put the host's name into `hosts`. A token
  works once and expires (after an hour by default, `--ttl` up to a day), so a host created
  later needs a new token. Once a host has joined, you can remove its token. The second
  host waits as passive; once the third joins, all three run stacks and vote. Joins need
  the applied `ClusterConfiguration`, because the leader checks the server with its Hetzner
  token. Its `privateNetwork`, `ingress.floatingIP` and `storageClasses` come from the
  output `cluster_configuration`. A host that stays `joining` in `clowder host list` never
  received the cluster's answer. Remove it with `clowder host remove`, put a new token into
  its `/etc/clowder/join-token`, and reboot it.
- **To replace a host**, add the new one to `hosts` and wait until it is active. Then run
  `clowder host remove` for the old one and take it out of `hosts`. A cluster that had
  three hosts keeps three. The module joins every host through the first one (`name`), so
  it cannot replace the first host yet, and no host can join while the first host is down.
- `clowderd_version` pins the version from clowder's APT repository, so that a host created
  later gets the cluster's version. Its default is the release that this module comes
  with, so check out the module at that release's tag.
- The module ignores later changes to a server's user data. So a storage class added to
  `storage_classes` gets a volume, but no thin pool, on a host that already exists.
- `clowderd_package_url` and `clowderd_package_sha256` are a test hook: they install a
  `.deb` instead of using the APT repository.
- `image` is the hosts' image: `debian-13` by default, or the ID of a snapshot that
  `setup.sh` prepared with `PREPARE_ONLY=yes` in its settings (the headers, the DRBD
  module, Docker and the rest of the dependencies of `clowderd`). On such a snapshot, the
  setup skips what is already there and installs `clowderd`; clowder's live tests start
  their hosts this way. A changed `image` applies only to hosts created later; it replaces
  no server.
