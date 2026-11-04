# A clowder cluster on Hetzner Cloud: the first host (var.name) and the hosts
# that join it (var.hosts), each a server with its volumes (one per storage
# class), in one private network behind one Cloud firewall, and the
# cluster's Floating IP. cloud-init writes setup.sh and its settings; setup.sh
# prepares a host and runs clowderd init (the first) or clowderd join (the
# others, each with a join token from clowder host join-token).

provider "hcloud" {
  # the token comes from HCLOUD_TOKEN
}

resource "random_password" "bootstrap" {
  # the key that lets the user in, once: the only defense of the bootstrap
  # route, so long, and plain enough for YAML, a shell and a file
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

# The first host.

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
  image        = var.image
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.host.id]
  user_data    = local.first_user_data
  labels       = { clowder = var.name }

  network {
    network_id = hcloud_network.hosts.id
  }

  lifecycle {
    # a change of the user data, the keys or the image would replace the server
    # and its data (a new image is for hosts made later); a storage class
    # added later therefore never reaches this host by itself. Hetzner's
    # provider keeps no power state, so clowder powering hosts off and on
    # (fencing) does not fight OpenTofu. The network stays as it was made:
    # the provider detaches and attaches it again at a later apply, which
    # gives the server another private address, and a host's underlay must
    # never change under the cluster.
    ignore_changes = [user_data, ssh_keys, network, image]
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

# The hosts that join: the same as the first, with a join token instead of
# the bootstrap key, through the first host as their seed.

resource "hcloud_volume" "joining" {
  for_each = local.joining_volumes

  name     = "${each.value.host}-${each.value.class}"
  size     = var.storage_classes[each.value.class]
  location = var.location
}

resource "hcloud_server" "joining" {
  for_each = toset(var.hosts)

  name         = each.key
  server_type  = var.server_type
  image        = var.image
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.host.id]
  user_data    = local.joining_user_data[each.key]
  labels       = { clowder = var.name }

  network {
    network_id = hcloud_network.hosts.id
  }

  lifecycle {
    # the token in the user data opens once; a new one must not replace the
    # server. The network and the image stay as they were made, as the first
    # host's do.
    ignore_changes = [user_data, ssh_keys, network, image]
  }

  depends_on = [hcloud_network_subnet.hosts]
}

resource "hcloud_volume_attachment" "joining" {
  for_each = local.joining_volumes

  volume_id = hcloud_volume.joining[each.key].id
  server_id = hcloud_server.joining[each.value.host].id
  automount = false
}

locals {
  # each joining host's volumes, by "<host>-<class>"
  joining_volumes = {
    for pair in setproduct(var.hosts, keys(var.storage_classes)) :
    "${pair[0]}-${pair[1]}" => { host = pair[0], class = pair[1] }
  }

  # setup.sh sources its settings; every value is quoted for the shell
  common_settings = [
    "FLOATING_IP='${hcloud_floating_ip.ingress.ip_address}'",
    "CLOWDERD_VERSION='${var.clowderd_version}'",
    "PACKAGE_URL='${var.clowderd_package_url}'",
    "PACKAGE_SHA256='${var.clowderd_package_sha256}'",
  ]
  setup_files = [
    { path = "/usr/local/sbin/clowder-setup", permissions = "0755", content = file("${path.module}/setup.sh") },
    { path = "/etc/systemd/system/clowder-setup.service", permissions = "0644", content = file("${path.module}/clowder-setup.service") },
  ]
  # --no-block: the setup runs after cloud-init (it may reboot in between),
  # not inside it
  setup_commands = [
    ["systemctl", "daemon-reload"],
    ["systemctl", "enable", "--no-block", "--now", "clowder-setup.service"],
  ]

  first_user_data = "#cloud-config\n${yamlencode({
    write_files = concat(local.setup_files, [
      {
        path = "/etc/clowder-setup.env", permissions = "0600",
        content = join("\n", concat(local.common_settings, [
          "VOLUMES='${join(" ", [for name, volume in hcloud_volume.class : "${name}=${volume.id}"])}'",
          "",
        ]))
      },
      { path = "/etc/clowder/bootstrap-key", permissions = "0600", content = "${random_password.bootstrap.result}\n" },
    ])
    runcmd = local.setup_commands
  })}"

  joining_user_data = {
    for host in var.hosts : host => "#cloud-config\n${yamlencode({
      write_files = concat(local.setup_files, [
        {
          path = "/etc/clowder-setup.env", permissions = "0600",
          content = join("\n", concat(local.common_settings, [
            "VOLUMES='${join(" ", [for key, volume in local.joining_volumes : "${volume.class}=${hcloud_volume.joining[key].id}" if volume.host == host])}'",
            "SEED='${one(hcloud_server.host.network).ip}'",
            "",
          ]))
        },
        { path = "/etc/clowder/join-token", permissions = "0600", content = "${lookup(var.join_tokens, host, "")}\n" },
      ])
      runcmd = local.setup_commands
    })}"
  }
}
