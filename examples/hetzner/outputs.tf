output "public_address" {
  description = "The first host's public IPv4: the first credentials come from it, ssh root@<address> clowderd credentials."
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

output "cluster_yaml" {
  description = "What cluster.yaml needs from this module."
  value = {
    private_network = var.network_ip_range
    ingress         = { floating_ip = hcloud_floating_ip.ingress.ip_address }
    storage_classes = { for name in keys(var.storage_classes) : name => { pool = "vg-${name}/thin" } }
  }
}
