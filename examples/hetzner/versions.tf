terraform {
  required_version = "~> 1.8"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.54"
    }
  }
}
