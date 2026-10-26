variable "region" { default = "us-east-1" }
variable "zone" { default = "us-east-1a" }
variable "subnet" {
  description = "the hosts' and coordinator's addresses (the VPC is this one subnet); the nodes live in 10.100.0.0/16 and up, one /16 per host"
  default     = "172.20.0.0/20"
}
variable "hosts" {
  description = "host instances; testbed fleet <scenario> prints the count (aws_packed_machines)"
  default     = 1
}
variable "instance_type" { default = "m6a.2xlarge" }
variable "coordinator_type" { default = "m6a.xlarge" }
variable "host_disk_gb" { default = 20 }
variable "coordinator_disk_gb" {
  description = "holds every trace and metrics.json while the coordinator aggregates (about 4 MB per node)"
  default     = 300
}
variable "max_hours" {
  description = "the coordinator terminates the hosts and halts this long after boot; 0 disables"
  default     = 6
}
