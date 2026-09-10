variable "project" {}
variable "region" { default = "us-central1" }
variable "zone" { default = "us-central1-a" }
variable "hosts" { default = 10 }
variable "nodes_per_host" { default = 1000 }
variable "instance_type" { description = "Tau T2A (Ampere arm64): 16 vCPU / 64 GB"; default = "t2a-standard-16" }
variable "coordinator_type" { default = "t2a-standard-2" }
variable "spot" { default = false }
variable "binaries_url" {}
