variable "regions" {
  description = "instances per region, one node each; from `testbed fleet <scenario>`"
  type        = map(number)
  default     = { us-east-1 = 1000 }
  validation {
    condition     = alltrue([for r in keys(var.regions) : contains(["us-east-1", "us-west-2", "eu-central-1", "ap-southeast-1", "sa-east-1"], r)])
    error_message = "unsupported region; add it to gen.py and regenerate"
  }
}
variable "home_region" {
  description = "coordinator, bootnode and binaries bucket"
  default     = "us-east-1"
}
variable "instance_type" {
  description = "one node per instance; t4g.nano (2 vCPU, 0.5 GB) is enough for a discv5+RLPx node"
  default     = "t4g.nano"
}
variable "coordinator_type" {
  default = "m7g.large"
}
variable "spot" {
  description = "spot for nodes; not for 24h churn runs unless reclaims are logged as departures"
  default     = false
}
