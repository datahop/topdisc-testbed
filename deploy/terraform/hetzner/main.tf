# Packed hosts on Hetzner Cloud, the Google Cloud layout: a server per
# `vnodes_per_machine` nodes, each node in its own netns on the host's bridge
# (pkg/host), plus a coordinator. Host i owns 10.(100+i).0.0/16 and a network
# route sends that /16 to it; the hosts forward. Hetzner has no private-only
# servers, so each keeps a public IPv4 (the driver pushes the binaries over
# it); the firewall allows SSH only, and testbed traffic stays on the private
# network, which firewalls do not touch.
terraform {
  required_providers {
    hcloud = { source = "hetznercloud/hcloud", version = "~> 1.45" }
  }
}
provider "hcloud" {
  token = var.hcloud_token
}

locals {
  network_zone = { fsn1 = "eu-central", nbg1 = "eu-central", hel1 = "eu-central", ash = "us-east", hil = "us-west", sin = "ap-southeast" }[var.location]
  cloud_init   = templatefile("${path.module}/../../hetzner-cloud-init.yaml.tftpl", {})
}

resource "hcloud_network" "tb" {
  name     = "topdisc-testbed"
  ip_range = var.subnet
}
resource "hcloud_network_subnet" "hosts" {
  network_id   = hcloud_network.tb.id
  type         = "cloud"
  network_zone = local.network_zone
  ip_range     = var.subnet
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

resource "hcloud_server" "host" {
  count        = var.hosts
  name         = "topdisc-host-${count.index}"
  server_type  = var.server_type
  image        = "debian-12"
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.admin.id]
  user_data    = local.cloud_init
  network {
    network_id = hcloud_network.tb.id
  }
  labels     = { topdisc = "host" }
  depends_on = [hcloud_network_subnet.hosts]
}
resource "hcloud_network_route" "host" {
  count       = var.hosts
  network_id  = hcloud_network.tb.id
  destination = "10.${100 + count.index}.0.0/16"
  gateway     = one(hcloud_server.host[count.index].network).ip
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
  labels     = { topdisc = "coordinator" }
  depends_on = [hcloud_network_subnet.hosts]
}
