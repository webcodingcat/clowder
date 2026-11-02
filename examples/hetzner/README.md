# The first host on Hetzner Cloud

An OpenTofu module for the first host of a clowder cluster: the server (Debian 13), a
private network, the Floating IP, one volume per storage class and a Cloud firewall. Its
cloud-init installs DRBD 9 from LINBIT and `clowderd` from clowder's APT repository,
prepares the host, checks it with `clowderd check` and runs `clowderd init`.
How to use it is in the main [README](../../README.md#getting-started-on-hetzner-cloud).

- **The Cloud firewall is the only filter** in front of SSH and clowder's API (port 7443):
  `admin_cidrs` names who reaches them. It takes no `0.0.0.0/0`. HTTP and HTTPS are open to
  everyone (Traefik, and the ACME CA's checks); a port your stacks publish opens with
  `published_ports` (`["udp/9987"]`).
- **The bootstrap key** lets the user in and later hosts join. It is in the OpenTofu
  state, encrypted in `cluster.yaml` (`join_keys`), and in the server's user data. Root
  reads the user data from cloud-init's cache; the metadata service hands it to any process
  on the host, root or not, and to containers in the host's network such as Traefik. Only
  containers on clowder's and Docker's bridges cannot reach it. So treat the key as known
  to everything that runs on the host itself. The setup deletes only its own copy in
  `/etc/clowder`. Keep the state as safe as the recovery key.
- `clowderd_version` pins the version from clowder's APT repository, so a host made later
  gets the cluster's version. Its default is the release this module comes with; check the
  module out at that release's tag.
- The module makes one host; more come with `clowderd join` (not there yet). It ignores later
  changes to the user data, so a storage class added to `storage_classes` gets a volume but
  no thin pool on the host.
- `clowderd_package_url` and `clowderd_package_sha256` are a test hook: they install a
  `.deb` instead of the APT repository.
