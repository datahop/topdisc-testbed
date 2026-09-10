variable "project" {}
variable "region" { default = "us-central1" }
variable "zone" { default = "us-central1-a" }
variable "nodes" {
  description = "instances, one node each; raise the CPU quota for 10k"
  default     = 1000
}
variable "instance_type" {
  description = "one node per instance; t2a-standard-1 is the smallest arm64 (no arm64 shared-core type); e2-micro for amd64 builds"
  default     = "t2a-standard-1"
}
variable "coordinator_type" { default = "t2a-standard-2" }
variable "spot" { default = false }
variable "binaries_url" {}
