variable "hcloud_token" { sensitive = true }
variable "location" { default = "fsn1" }
variable "network_zone" { default = "eu-central" }
variable "nodes" { description = "servers, one node each; the account server limit must cover it"; default = 1000 }
variable "instance_type" { description = "one node per server; cax11 is arm64 (2 vCPU, 4 GB), cx22 amd64"; default = "cax11" }
variable "coordinator_type" { default = "cpx21" }
variable "admin_cidr" { description = "who may SSH to the public addresses" }
variable "ssh_public_key" {}
variable "binaries_url" {}
