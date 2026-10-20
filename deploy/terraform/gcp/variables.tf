variable "project" {}
variable "region" { default = "us-central1" }
variable "zone" { default = "us-central1-a" }
variable "subnet" {
  description = "the hosts' and coordinator's addresses; the nodes live in 10.100.0.0/16 and up, one /16 per host"
  default     = "172.20.0.0/20"
}
variable "hosts" {
  description = "host VMs; testbed fleet <scenario> prints the count (gcp_machines)"
  default     = 1
}
variable "machine_type" { default = "n2-standard-8" }
variable "coordinator_type" { default = "e2-standard-4" }
variable "host_disk_gb" { default = 20 }
variable "coordinator_disk_gb" {
  description = "holds every trace while the coordinator aggregates (about 2 MB per node)"
  default     = 100
}
variable "max_hours" {
  description = "the coordinator deletes the hosts and halts this long after boot; 0 disables"
  default     = 6
}
