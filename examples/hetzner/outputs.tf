output "public_address" {
  description = "The host's public IPv4: the seed for clowder cluster bootstrap."
  value       = hcloud_server.host.ipv4_address
}

output "private_address" {
  description = "The host's address in the private network (its underlay)."
  value       = one(hcloud_server.host.network).ip
}

output "server_id" {
  description = "Hetzner's ID of the server."
  value       = hcloud_server.host.id
}

output "floating_ip" {
  description = "The cluster's Floating IP."
  value       = hcloud_floating_ip.ingress.ip_address
}

output "bootstrap_key" {
  description = "The cluster's bootstrap key, for clowder cluster bootstrap --key-file. The OpenTofu state, the server's user data and cluster.yaml (encrypted) hold it too."
  value       = random_password.bootstrap.result
  sensitive   = true
}

output "cluster_yaml" {
  description = "What cluster.yaml needs from this module."
  value = {
    seeds           = [hcloud_server.host.ipv4_address]
    network         = "private"
    ingress         = { floating_ip = hcloud_floating_ip.ingress.ip_address }
    storage_classes = { for name in keys(var.storage_classes) : name => { pool = "vg-${name}/thin" } }
  }
}
