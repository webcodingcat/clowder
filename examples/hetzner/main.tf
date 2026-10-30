# One clowder host on Hetzner Cloud: the server, its private network, its
# volumes (one per storage class), the cluster's Floating IP and a Cloud
# firewall. cloud-init writes setup.sh and its settings; setup.sh prepares the
# host and runs clowderd init.
#
# One host only: more come with clowderd join (phase 2). Until then a second
# server would found a cluster of its own.

provider "hcloud" {
  # the token comes from HCLOUD_TOKEN
}

resource "random_password" "bootstrap" {
  # the key that lets the first user in and later hosts join: the only defence
  # of the bootstrap route, so long, and plain enough for YAML, a shell and a file
  length  = 48
  special = false
}

resource "hcloud_ssh_key" "admin" {
  name       = var.name
  public_key = var.ssh_public_key
}

resource "hcloud_network" "hosts" {
  name     = var.name
  ip_range = var.network_ip_range
}

resource "hcloud_network_subnet" "hosts" {
  network_id   = hcloud_network.hosts.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = var.network_ip_range
}

# The firewall also covers the Floating IP, where stacks publish their ports
# (phase 1b). WireGuard runs over the private network, which Cloud firewalls do
# not filter.
resource "hcloud_firewall" "host" {
  name = var.name

  rule {
    description = "SSH"
    direction   = "in"
    protocol    = "tcp"
    port        = "22"
    source_ips  = var.admin_cidrs
  }

  rule {
    description = "clowder's API"
    direction   = "in"
    protocol    = "tcp"
    port        = "7443"
    source_ips  = var.admin_cidrs
  }

  # Traefik on the Floating IP; the ACME CA validates its challenges over
  # HTTP from anywhere
  rule {
    description = "HTTP"
    direction   = "in"
    protocol    = "tcp"
    port        = "80"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  rule {
    description = "HTTPS"
    direction   = "in"
    protocol    = "tcp"
    port        = "443"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  dynamic "rule" {
    for_each = var.published_ports
    content {
      description = "published ${rule.value}"
      direction   = "in"
      protocol    = split("/", rule.value)[0]
      port        = split("/", rule.value)[1]
      source_ips  = ["0.0.0.0/0", "::/0"]
    }
  }
}

# Created apart from the server and attached later, so the server's user data
# can name the Floating IP and the volumes without a dependency cycle.
resource "hcloud_floating_ip" "ingress" {
  name          = var.name
  type          = "ipv4"
  home_location = var.location
}

resource "hcloud_volume" "class" {
  for_each = var.storage_classes

  name     = "${var.name}-${each.key}"
  size     = each.value
  location = var.location
  # no format: setup.sh makes an LVM thin pool on the bare device
}

resource "hcloud_server" "host" {
  name         = var.name
  server_type  = var.server_type
  image        = "debian-13"
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.host.id]
  user_data    = local.user_data
  labels       = { clowder = var.name }

  network {
    network_id = hcloud_network.hosts.id
  }

  lifecycle {
    # either change would replace the server and its data; a storage class
    # added later therefore never reaches this host by itself. Hetzner's
    # provider keeps no power state, so clowder powering hosts off and on
    # (fencing) does not fight OpenTofu.
    ignore_changes = [user_data, ssh_keys]
  }

  depends_on = [hcloud_network_subnet.hosts]
}

resource "hcloud_floating_ip_assignment" "ingress" {
  floating_ip_id = hcloud_floating_ip.ingress.id
  server_id      = hcloud_server.host.id

  lifecycle {
    # clowder moves the Floating IP to whichever host holds the ingress
    ignore_changes = [server_id]
  }
}

resource "hcloud_volume_attachment" "class" {
  for_each = var.storage_classes

  volume_id = hcloud_volume.class[each.key].id
  server_id = hcloud_server.host.id
  automount = false
}

locals {
  # setup.sh sources this file; every value is quoted for the shell
  setup_env = join("\n", [
    "NAME='${var.name}'",
    "FLOATING_IP='${hcloud_floating_ip.ingress.ip_address}'",
    "VOLUMES='${join(" ", [for name, volume in hcloud_volume.class : "${name}=${volume.id}"])}'",
    "CLOWDERD_VERSION='${var.clowderd_version}'",
    "PACKAGE_URL='${var.clowderd_package_url}'",
    "PACKAGE_SHA256='${var.clowderd_package_sha256}'",
    "",
  ])

  user_data = "#cloud-config\n${yamlencode({
    write_files = [
      { path = "/usr/local/sbin/clowder-setup", permissions = "0755", content = file("${path.module}/setup.sh") },
      { path = "/etc/systemd/system/clowder-setup.service", permissions = "0644", content = file("${path.module}/clowder-setup.service") },
      { path = "/etc/clowder-setup.env", permissions = "0600", content = local.setup_env },
      { path = "/etc/clowder/bootstrap-key", permissions = "0600", content = "${random_password.bootstrap.result}\n" },
    ]
    # --no-block: the setup runs after cloud-init (it may reboot in between),
    # not inside it
    runcmd = [
      ["systemctl", "daemon-reload"],
      ["systemctl", "enable", "--no-block", "--now", "clowder-setup.service"],
    ]
  })}"
}
