# Three hosts in one location and a private network, a Floating IP on node0,
# two volumes per host (stand-ins for the storage classes bulk and fast).

terraform {
  required_version = "~> 1.8"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.54"
    }
  }
}

provider "hcloud" {
  # endpoint and token come from HCLOUD_ENDPOINT / HCLOUD_TOKEN (`hlab env`)
  poll_interval = "500ms"
}

variable "ssh_public_key" {
  type        = string
  description = "Public key that the hosts accept for root (run.sh uses the lab's own key)."
}

locals {
  nodes = { node0 = 0, node1 = 1, node2 = 2 }
}

resource "hcloud_ssh_key" "admin" {
  name       = "clowder-spike"
  public_key = var.ssh_public_key
}

resource "hcloud_network" "net" {
  name     = "clowder-spike"
  ip_range = "10.0.0.0/16"
}

resource "hcloud_network_subnet" "net" {
  network_id   = hcloud_network.net.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = "10.0.1.0/24"
}

resource "hcloud_server" "node" {
  for_each = local.nodes

  name        = each.key
  server_type = "cx23"
  image       = "debian-13"
  location    = "nbg1"
  ssh_keys    = [hcloud_ssh_key.admin.id]
  labels      = { cluster = "clowder-spike" }

  network {
    network_id = hcloud_network.net.id
    ip         = "10.0.1.${10 + each.value}"
    alias_ips  = []
  }

  # clowder powers hosts off and on (fencing); a new user_data would replace
  # the server.
  lifecycle {
    ignore_changes = [user_data]
  }

  depends_on = [hcloud_network_subnet.net]
}

resource "hcloud_volume" "bulk" {
  for_each = local.nodes

  name      = "${each.key}-bulk"
  size      = 10
  server_id = hcloud_server.node[each.key].id
  automount = false
}

resource "hcloud_volume" "fast" {
  for_each = local.nodes

  name      = "${each.key}-fast"
  size      = 10
  server_id = hcloud_server.node[each.key].id
  automount = false
}

resource "hcloud_floating_ip" "ingress" {
  type          = "ipv4"
  home_location = "nbg1"
  name          = "clowder-spike"
  server_id     = hcloud_server.node["node0"].id

  # clowder moves the Floating IP at runtime
  lifecycle {
    ignore_changes = [server_id]
  }
}

output "nodes" {
  description = "Server ID, public and private address and volume IDs of each host."
  value = {
    for name, server in hcloud_server.node : name => {
      id      = server.id
      public  = server.ipv4_address
      private = one(server.network).ip
      bulk    = hcloud_volume.bulk[name].id
      fast    = hcloud_volume.fast[name].id
    }
  }
}

output "floating_ip" {
  description = "ID and address of the Floating IP."
  value = {
    id      = hcloud_floating_ip.ingress.id
    address = hcloud_floating_ip.ingress.ip_address
  }
}
