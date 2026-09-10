# Packed fleet on Hetzner Cloud: dedicated-vCPU CCX hosts on a private network.
# Hetzner has no private-only instances, so hosts keep a public IPv4 and the
# firewall allows only SSH from var.admin_cidr; testbed traffic stays private.
terraform {
  required_providers { hcloud = { source = "hetznercloud/hcloud", version = "~> 1.45" } }
}
provider "hcloud" { token = var.hcloud_token }

resource "hcloud_network" "tb" { name = "topdisc-testbed"; ip_range = "10.0.0.0/8" }
resource "hcloud_network_subnet" "nodes" { network_id = hcloud_network.tb.id; type = "cloud"; network_zone = var.network_zone; ip_range = "10.10.0.0/18" }
resource "hcloud_firewall" "admin" {
  name = "topdisc-admin"
  rule { direction = "in"; protocol = "tcp"; port = "22"; source_ips = [var.admin_cidr] }
}
resource "hcloud_ssh_key" "admin" { name = "topdisc-admin"; public_key = var.ssh_public_key }

locals {
  cloud_init = templatefile("${path.module}/../../cloud-init.yaml.tftpl", { binaries_url = var.binaries_url, nodes_per_host = var.nodes_per_host })
}

resource "hcloud_server" "coordinator" {
  name = "topdisc-coordinator"; server_type = var.coordinator_type; image = "debian-12"; location = var.location
  ssh_keys = [hcloud_ssh_key.admin.id]; firewall_ids = [hcloud_firewall.admin.id]; user_data = local.cloud_init
  network { network_id = hcloud_network.tb.id }
  labels = { role = "coordinator" }
  depends_on = [hcloud_network_subnet.nodes]
}

resource "hcloud_server" "host" {
  count = var.hosts
  name = "topdisc-host-${count.index}"; server_type = var.instance_type; image = "debian-12"; location = var.location
  ssh_keys = [hcloud_ssh_key.admin.id]; firewall_ids = [hcloud_firewall.admin.id]; user_data = local.cloud_init
  network { network_id = hcloud_network.tb.id }
  labels = { role = "host", index = count.index }
  depends_on = [hcloud_network_subnet.nodes]
}

resource "hcloud_network_route" "host_subnet" {
  count = var.hosts
  network_id = hcloud_network.tb.id
  destination = "10.${100 + count.index}.0.0/16"
  gateway = one(hcloud_server.host[count.index].network).ip
}
