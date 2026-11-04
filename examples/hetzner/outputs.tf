output "public_address" {
  description = "The first host's public IPv4: --endpoint of clowder cluster bootstrap."
  value       = hcloud_server.host.ipv4_address
}

output "private_address" {
  description = "The first host's address in the private network (its underlay); the joining hosts join through it."
  value       = one(hcloud_server.host.network).ip
}

output "server_id" {
  description = "Hetzner's ID of the first host's server."
  value       = hcloud_server.host.id
}

output "hosts" {
  description = "Every host by name: its public and private address and its server's ID."
  value = merge(
    {
      (var.name) = {
        public_address  = hcloud_server.host.ipv4_address
        private_address = one(hcloud_server.host.network).ip
        server_id       = hcloud_server.host.id
      }
    },
    {
      for name, server in hcloud_server.joining : name => {
        public_address  = server.ipv4_address
        private_address = one(server.network).ip
        server_id       = server.id
      }
    },
  )
}

output "floating_ip" {
  description = "The cluster's Floating IP."
  value       = hcloud_floating_ip.ingress.ip_address
}

output "bootstrap_key" {
  description = "The cluster's bootstrap key, for clowder cluster bootstrap --key-file; it opens nothing after that. The OpenTofu state and the first server's user data hold it too."
  value       = random_password.bootstrap.result
  sensitive   = true
}

output "cluster_yaml" {
  description = "What cluster.yaml needs from this module."
  value = {
    # every host: the CLI tries them in turn and learns no others
    endpoints = concat(
      [hcloud_server.host.ipv4_address],
      [for server in hcloud_server.joining : server.ipv4_address],
    )
    private_network = var.network_ip_range
    ingress         = { floating_ip = hcloud_floating_ip.ingress.ip_address }
    storage_classes = { for name in keys(var.storage_classes) : name => { pool = "vg-${name}/thin" } }
  }
}
