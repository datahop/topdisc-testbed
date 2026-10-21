# Packed hosts, as on Grid'5000: a VM per `vnodes_per_machine` nodes, each
# node in its own netns on the host's bridge (pkg/host), plus a coordinator.
# Host i owns 10.(100+i).0.0/16 and a VPC route sends that /16 to it; the
# hosts carry IP forwarding so the VPC delivers packets addressed to their
# nodes. Private addresses only: the binaries come from the bucket over
# Private Google Access, SSH goes through IAP.
terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "~> 6.0" }
    random = { source = "hashicorp/random", version = "~> 3.0" }
  }
}
provider "google" {
  project = var.project
  region  = var.region
  zone    = var.zone
}

resource "google_compute_network" "tb" {
  name                    = "topdisc-testbed"
  auto_create_subnetworks = false
  mtu                     = 1500
}
resource "google_compute_subnetwork" "hosts" {
  name                     = "topdisc-hosts"
  network                  = google_compute_network.tb.id
  ip_cidr_range            = var.subnet
  private_ip_google_access = true
}
resource "google_compute_firewall" "intra" {
  name    = "topdisc-intra"
  network = google_compute_network.tb.name
  allow { protocol = "all" }
  source_ranges = [var.subnet, "10.0.0.0/8"]
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

# Binaries in, run archives out. Destroyed with the deployment: gcp.sh down
# copies the archives first. A deleted bucket's name stays taken for a
# while, so each deployment gets its own.
resource "random_id" "bucket" {
  byte_length = 3
}
resource "google_storage_bucket" "tb" {
  name                        = "${var.project}-topdisc-${random_id.bucket.hex}"
  location                    = var.region
  force_destroy               = true
  uniform_bucket_level_access = true
}

resource "google_service_account" "host" {
  account_id   = "topdisc-host"
  display_name = "topdisc host"
}
resource "google_service_account" "coordinator" {
  account_id   = "topdisc-coordinator"
  display_name = "topdisc coordinator"
}
resource "google_storage_bucket_iam_member" "host" {
  bucket = google_storage_bucket.tb.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.host.email}"
}
resource "google_storage_bucket_iam_member" "coordinator" {
  bucket = google_storage_bucket.tb.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.coordinator.email}"
}
# The coordinator lists the hosts for the inventory and deletes them, and
# only them, at the spending cap.
resource "google_project_iam_member" "coordinator_viewer" {
  project = var.project
  role    = "roles/compute.viewer"
  member  = "serviceAccount:${google_service_account.coordinator.email}"
}
resource "google_project_iam_member" "coordinator" {
  project = var.project
  role    = "roles/compute.instanceAdmin.v1"
  member  = "serviceAccount:${google_service_account.coordinator.email}"
  condition {
    title      = "topdisc hosts only"
    expression = "resource.name.startsWith(\"projects/${var.project}/zones/${var.zone}/instances/topdisc-host-\")"
  }
}

locals {
  image = "debian-cloud/debian-12"
  startup = { for role in ["host", "coordinator"] : role => templatefile("${path.module}/../../gcp-startup.sh.tftpl", {
    role = role, bucket = google_storage_bucket.tb.name, subnet = var.subnet, zone = var.zone, max_hours = var.max_hours
  }) }
}

resource "google_compute_instance" "host" {
  count          = var.hosts
  name           = "topdisc-host-${count.index}"
  zone           = var.zone
  machine_type   = var.machine_type
  can_ip_forward = true
  boot_disk {
    initialize_params {
      image = local.image
      size  = var.host_disk_gb
      type  = "pd-balanced"
    }
  }
  network_interface { subnetwork = google_compute_subnetwork.hosts.id }
  metadata = { startup-script = local.startup["host"] }
  service_account {
    email  = google_service_account.host.email
    scopes = ["cloud-platform"]
  }
  labels     = { topdisc = "host" }
  depends_on = [google_storage_bucket_iam_member.host]
}
resource "google_compute_route" "host" {
  count                  = var.hosts
  name                   = "topdisc-host-${count.index}"
  network                = google_compute_network.tb.name
  dest_range             = "10.${100 + count.index}.0.0/16"
  next_hop_instance      = google_compute_instance.host[count.index].self_link
  next_hop_instance_zone = var.zone
  priority               = 100
}

resource "google_compute_instance" "coordinator" {
  name         = "topdisc-coordinator"
  zone         = var.zone
  machine_type = var.coordinator_type
  boot_disk {
    initialize_params {
      image = local.image
      size  = var.coordinator_disk_gb
      type  = "pd-balanced"
    }
  }
  network_interface { subnetwork = google_compute_subnetwork.hosts.id }
  metadata = { startup-script = local.startup["coordinator"] }
  service_account {
    email  = google_service_account.coordinator.email
    scopes = ["cloud-platform"]
  }
  labels     = { topdisc = "coordinator" }
  depends_on = [google_storage_bucket_iam_member.coordinator, google_project_iam_member.coordinator, google_project_iam_member.coordinator_viewer]
}
