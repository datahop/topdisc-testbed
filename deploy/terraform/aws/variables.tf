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
variable "instance_types" {
  description = "one node per instance, cheapest first; several sizes so spot can fill across pools (all arm64)"
  default     = ["t4g.nano", "t4g.micro", "t4g.small"]
}
variable "coordinator_type" {
  default = "m7g.large"
}
variable "spot" {
  description = "spot for nodes; not for 24h churn runs unless reclaims are logged as departures"
  default     = false
}
variable "max_hours" {
  description = "the coordinator scales every autoscaling group to zero this long after boot"
  default     = 6
}
