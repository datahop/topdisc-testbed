variable "hcloud_token" { sensitive = true }
variable "location" { default = "fsn1" }
variable "network_zone" { default = "eu-central" }
variable "hosts" { default = 10 }
variable "nodes_per_host" { default = 1000 }
variable "instance_type" { description = "CCX43: 16 dedicated vCPU / 64 GB (x86; build binaries for linux/amd64)"; default = "ccx43" }
variable "coordinator_type" { default = "cpx21" }
variable "admin_cidr" { description = "who may SSH to the public addresses" }
variable "ssh_public_key" {}
variable "binaries_url" {}
