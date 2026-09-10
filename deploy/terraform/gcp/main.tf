# One node per instance: a managed instance group of N small instances plus a
# coordinator, internal IPs only (Cloud NAT for the binaries download), IAP
# for SSH.
terraform {
  required_providers { google = { source = "hashicorp/google", version = "~> 5.0" } }
}
provider "google" {
  project = var.project
  region  = var.region
  zone    = var.zone
}

resource "google_compute_network" "tb" {
  name                    = "topdisc-testbed"
  auto_create_subnetworks = false
}
resource "google_compute_subnetwork" "nodes" {
  name          = "topdisc-nodes"
  network       = google_compute_network.tb.id
  ip_cidr_range = "10.10.0.0/18"
}
resource "google_compute_firewall" "intra" {
  name    = "topdisc-intra"
  network = google_compute_network.tb.name
  allow { protocol = "all" }
  source_ranges = ["10.10.0.0/16"]
}
resource "google_compute_firewall" "iap" {
  name    = "topdisc-iap-ssh"
  network = google_compute_network.tb.name
  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
  source_ranges = ["35.235.240.0/20"]
}
resource "google_compute_router" "r" {
  name    = "topdisc-router"
  network = google_compute_network.tb.id
}
resource "google_compute_router_nat" "nat" {
  name                               = "topdisc-nat"
  router                             = google_compute_router.r.name
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

locals {
  cloud_init = templatefile("${path.module}/../../cloud-init.yaml.tftpl", { binaries_url = var.binaries_url, binaries_s3 = "", region = "" })
  image      = "debian-cloud/debian-12-arm64"
}

resource "google_compute_instance" "coordinator" {
  name         = "topdisc-coordinator"
  machine_type = var.coordinator_type
  boot_disk {
    initialize_params {
      image = local.image
      size  = 20
    }
  }
  network_interface { subnetwork = google_compute_subnetwork.nodes.id }
  metadata = { user-data = local.cloud_init }
  service_account { scopes = ["compute-ro"] } # lists the fleet for the inventory
  labels = { role = "coordinator" }
}

resource "google_compute_instance_template" "node" {
  name_prefix  = "topdisc-node-"
  machine_type = var.instance_type
  disk {
    source_image = local.image
    disk_size_gb = 10
  }
  network_interface { subnetwork = google_compute_subnetwork.nodes.id }
  metadata = { user-data = local.cloud_init }
  scheduling {
    preemptible        = var.spot
    automatic_restart  = !var.spot
    provisioning_model = var.spot ? "SPOT" : "STANDARD"
  }
  labels = { role = "host" }
  lifecycle { create_before_destroy = true }
}

resource "google_compute_instance_group_manager" "nodes" {
  name               = "topdisc-nodes"
  base_instance_name = "topdisc-node"
  target_size        = var.nodes
  version { instance_template = google_compute_instance_template.node.id }
}
