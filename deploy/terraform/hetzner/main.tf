# One node per instance on Hetzner Cloud: N small servers on a private
# network. Hetzner has no private-only servers, so each keeps a public IPv4;
# the firewall allows only SSH from var.admin_cidr and testbed traffic stays
# on the private network.
terraform {
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.45"
    }
  }
}
provider "hcloud" {
  token = var.hcloud_token
}

resource "hcloud_network" "tb" {
  name     = "topdisc-testbed"
  ip_range = "10.0.0.0/8"
}
resource "hcloud_network_subnet" "nodes" {
  network_id   = hcloud_network.tb.id
  type         = "cloud"
  network_zone = var.network_zone
  ip_range     = "10.10.0.0/18"
}
resource "hcloud_firewall" "admin" {
  name = "topdisc-admin"
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = [var.admin_cidr]
  }
}
resource "hcloud_ssh_key" "admin" {
  name       = "topdisc-admin"
  public_key = var.ssh_public_key
}

locals {
  cloud_init = templatefile("${path.module}/../../cloud-init.yaml.tftpl", { binaries_url = var.binaries_url, binaries_s3 = "", region = "", regions = "" })
}

resource "hcloud_server" "coordinator" {
  name         = "topdisc-coordinator"
  server_type  = var.coordinator_type
  image        = "debian-12"
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.admin.id]
  user_data    = local.cloud_init
  network {
    network_id = hcloud_network.tb.id
  }
  labels     = { role = "coordinator" }
  depends_on = [hcloud_network_subnet.nodes]
}

resource "hcloud_server" "host" {
  count        = var.nodes
  name         = "topdisc-node-${count.index}"
  server_type  = var.instance_type
  image        = "debian-12"
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.admin.id]
  user_data    = local.cloud_init
  network {
    network_id = hcloud_network.tb.id
  }
  labels     = { role = "host", index = count.index }
  depends_on = [hcloud_network_subnet.nodes]
}
