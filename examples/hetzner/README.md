# The first host on Hetzner Cloud

An OpenTofu module for the first host of a clowder cluster: the server (Debian 13), a
private network, the Floating IP, one volume per storage class and a Cloud firewall. Its
cloud-init installs DRBD 9 from LINBIT and `clowderd` from clowder's APT repository,
prepares the host, checks it with `clowderd check` and runs `clowderd init`.
How to use it is in the main [README](../../README.md#a-host-on-hetzner-cloud).

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
