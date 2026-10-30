variable "name" {
  type        = string
  description = "Name of the host; also the prefix of the other resources."
  default     = "clowder0"
  validation {
    # it also goes into setup.sh's settings, quoted for the shell
    condition     = can(regex("^[a-z][a-z0-9-]*$", var.name))
    error_message = "The name is lowercase letters, digits and -."
  }
}

variable "ssh_public_key" {
  type        = string
  description = "Public key that root on the host accepts."
}

variable "admin_cidrs" {
  type        = list(string)
  description = "Addresses that reach SSH (22) and clowder's API (7443); the Cloud firewall is the only filter there."
  validation {
    condition     = length(var.admin_cidrs) > 0 && !contains(var.admin_cidrs, "0.0.0.0/0") && !contains(var.admin_cidrs, "::/0")
    error_message = "Name the addresses of your admins, not the whole Internet."
  }
}

variable "published_ports" {
  type        = list(string)
  description = "Ports the stacks publish besides HTTP and HTTPS, as protocol/port (\"udp/9987\"); the Cloud firewall lets them in from anywhere."
  default     = []
  validation {
    condition = alltrue([for port in var.published_ports :
      can(regex("^(tcp|udp)/[0-9]{1,5}$", port)) && try(tonumber(split("/", port)[1]) >= 1 && tonumber(split("/", port)[1]) <= 65535, false)
    ])
    error_message = "Write each port as tcp/<port> or udp/<port>, the port from 1 to 65535."
  }
}

variable "location" {
  type        = string
  description = "Hetzner location of the host, its volumes and the Floating IP."
  default     = "nbg1"
  validation {
    # the private network's subnet is in the network zone eu-central
    condition     = contains(["fsn1", "nbg1", "hel1"], var.location)
    error_message = "A location in eu-central: fsn1, nbg1 or hel1."
  }
}

variable "server_type" {
  type        = string
  description = "Hetzner server type."
  default     = "cx23"
}

variable "network_ip_range" {
  type        = string
  description = "The private network between the hosts."
  default     = "10.0.0.0/16"
  validation {
    # Docker's address pools are 172.20.0.0/14 (setup.sh) and clowder's overlay
    # is 10.77.0.0/24 and 10.78.0.0/16: the network must stay clear of them
    condition = (
      startswith(var.network_ip_range, "10.") &&
      !startswith(var.network_ip_range, "10.77.") &&
      !startswith(var.network_ip_range, "10.78.") &&
      tonumber(split("/", var.network_ip_range)[1]) >= 16
    )
    error_message = "Use a /16 or smaller in 10.0.0.0/8, outside 10.77.0.0/16 and 10.78.0.0/16."
  }
}

variable "storage_classes" {
  type        = map(number)
  description = "One Hetzner volume per storage class (name: size in GB); setup.sh makes the thin pool vg-<name>/thin on it."
  default     = { fast = 20, bulk = 20 }
  validation {
    condition     = alltrue([for name in keys(var.storage_classes) : can(regex("^[a-z][a-z0-9-]*$", name))])
    error_message = "Storage class names are lowercase letters, digits and -."
  }
}

variable "clowderd_version" {
  type        = string
  description = "Version of clowderd to install from clowder's APT repository; empty for the newest. Pin it, so a host made later gets the cluster's version."
  default     = ""
}

variable "clowderd_package_url" {
  type        = string
  description = "Test hook: a clowderd .deb to install instead of the APT repository (clowder's live tests)."
  default     = ""
}

variable "clowderd_package_sha256" {
  type        = string
  description = "Test hook: the SHA-256 of clowderd_package_url's file."
  default     = ""
  validation {
    condition     = var.clowderd_package_sha256 == "" || can(regex("^[0-9a-f]{64}$", var.clowderd_package_sha256))
    error_message = "A SHA-256 in lowercase hex."
  }
}
