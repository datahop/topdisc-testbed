variable "region" { default = "us-east-1" }
variable "az" { default = "us-east-1a" }
variable "nodes" {
  description = "instances, one node each; 10k needs the vCPU quota raised (t4g.nano = 2 vCPU)"
  default     = 1000
}
variable "instance_type" {
  description = "one node per instance; t4g.nano (2 vCPU, 0.5 GB) is enough for a discv5+RLPx node"
  default     = "t4g.nano"
}
variable "coordinator_type" { default = "m7g.large" }
variable "spot" {
  description = "spot for nodes; not for 24h churn runs unless reclaims are logged as departures"
  default     = false
}
