# A cluster on Hetzner Cloud

An OpenTofu module for a clowder cluster: the first host (`name`) and the hosts that join
it (`hosts`), each a server (Debian 13) with one volume per storage class, in one private
network behind one Cloud firewall, and the cluster's Floating IP. Its cloud-init installs
DRBD 9 from LINBIT and `clowderd` from clowder's APT repository, prepares the host, checks
it with `clowderd check` and runs `clowderd init` on the first host and `clowderd join` on
the others. How to use it is in the main [README](../../README.md#getting-started-on-hetzner-cloud).

- **One Hetzner project per cluster.** The hoster's token in `cluster.yaml` may power off
  and on any server of its project, and fencing does. Keep other servers out of the
  cluster's project. A joining host names its server itself; the leader checks with
  Hetzner that the server's private address is the host's, and takes no server twice.
- **The Cloud firewall is the only filter** in front of SSH and clowder's API (port 7443):
  `admin_cidrs` names who reaches them. It takes no `0.0.0.0/0`. HTTP and HTTPS are open to
  everyone (Traefik, and the ACME CA's checks); a port your stacks publish opens with
  `published_ports` (`["udp/9987"]`).
- **The bootstrap key** lets the first user in, once (`clowder cluster bootstrap`); after
  that it opens nothing. It is in the OpenTofu state and the first server's user data,
  which root reads from cloud-init's cache and the metadata service hands to any process
  that reaches it. Once `clowderd` runs, its rules close the service to the host and its
  containers, but at boot cloud-init reads it before `clowderd` starts. So bootstrap
  soon after the first host is up. The setup reads the service once, keeps what it needs
  in `/var/lib/clowder-setup` (the private networks only once they list an address),
  and deletes its own copy of the key in `/etc/clowder`.
- **More hosts** join with a token each: `clowder host join-token` prints one, which goes
  into `join_tokens` under the host's name, and the host's name into `hosts`. A token
  opens once and expires (an hour by default, `--ttl` up to a day), so a host made later
  needs a new one; once a host has joined, its token may go. The second host waits
  passive; with the third, all three run stacks and vote. Joins need the applied
  `cluster.yaml`, as the leader checks the server with the hoster's token. A host that
  stays `joining` in `clowder host list` never got the cluster's answer: remove it with
  `clowder host remove`, put a new token into its `/etc/clowder/join-token` and reboot it.
- **To replace a host**, add the new one to `hosts`, wait until it is active, then
  `clowder host remove` the old one and take it out of `hosts`. A cluster that had three
  hosts keeps three. The module joins every host through the first one (`name`): it
  cannot replace that one yet, and while it is down no host joins.
- `clowderd_version` pins the version from clowder's APT repository, so a host made later
  gets the cluster's version. Its default is the release this module comes with; check the
  module out at that release's tag.
- The module ignores later changes to a server's user data, so a storage class added to
  `storage_classes` gets a volume but no thin pool on a host that exists.
- `clowderd_package_url` and `clowderd_package_sha256` are a test hook: they install a
  `.deb` instead of the APT repository.
- `image` is the hosts' image: `debian-13` by default, or the ID of a snapshot that
  `setup.sh` prepared with `PREPARE_ONLY=yes` in its settings (the headers, DRBD's module,
  Docker and the rest of what `clowderd` depends on). On such a snapshot the setup skips
  what is there and installs `clowderd`; clowder's live tests start their hosts so. A
  changed `image` is for hosts made later; it replaces no server.
