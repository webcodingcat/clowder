variable "name" {
  type        = string
  description = "Name of the first host; also the prefix of the other resources."
  default     = "clowder0"
  validation {
    # it also goes into setup.sh's settings, quoted for the shell
    condition     = can(regex("^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$", var.name))
    error_message = "The name is lowercase letters, digits and -, at most 63, starting with a letter and not ending with -."
  }
}

variable "cluster_name" {
  type        = string
  description = "The cluster's name (clowderd init --cluster-name): cluster status shows it, alerts and new credentials carry it."
  default     = "clowder-cluster"
  validation {
    # clowderd init's rule (internal/names, Valid), checked here before a
    # server is made; it also goes into setup.sh's settings, quoted for the
    # shell
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", var.cluster_name))
    error_message = "The cluster's name is lowercase letters, digits and -, at most 63, starting and ending with a letter or digit."
  }
}

variable "hosts" {
  type        = list(string)
  description = "Names of the hosts that join the first one, each with a token in join_tokens."
  default     = []
  validation {
    # clowder takes a host's name as a DNS label, as it does the first's
    condition     = alltrue([for host in var.hosts : can(regex("^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$", host))]) && length(distinct(var.hosts)) == length(var.hosts)
    error_message = "Each host's name is lowercase letters, digits and -, at most 63, starting with a letter and not ending with -, and once."
  }
}

variable "roles" {
  type        = map(string)
  description = "The roles of a host (clowderd init and join --roles), by its name: a comma-separated list of control-plane, worker and ingress. A host not named has all three. The first host needs control-plane; a joining host's token must allow its roles (clowder host join-token --roles). A host without worker gets no volumes."
  default     = {}
  validation {
    condition = alltrue([
      for host, roles in var.roles :
      contains(concat([var.name], var.hosts), host) &&
      alltrue([for role in split(",", roles) : contains(["control-plane", "worker", "ingress"], role)]) &&
      length(distinct(split(",", roles))) == length(split(",", roles))
    ])
    error_message = "Each key is the first host or one of hosts, and each value lists control-plane, worker and ingress, each at most once, without spaces."
  }
  validation {
    condition     = contains(split(",", lookup(var.roles, var.name, "control-plane")), "control-plane")
    error_message = "The first host needs the role control-plane."
  }
}

variable "join_tokens" {
  type        = map(string)
  description = "A join token (clowder host join-token) per host of hosts that has not joined yet. It opens once and expires: a host made later needs a new one; for one that joined, the token may go."
  default     = {}
  sensitive   = true
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
  description = "Version of clowderd to install from clowder's APT repository (0.1.0 for the tag v0.1.0). It stays pinned, so a host made later gets the cluster's version; empty takes the newest."
  default     = "0.1.0"
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

variable "image" {
  type        = string
  description = "The hosts' image: debian-13, or the ID of a snapshot prepared from it (setup.sh with PREPARE_ONLY), which spares the setup the packages and DRBD's module."
  default     = "debian-13"
}
