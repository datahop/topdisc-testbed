variable "region" { default = "us-east-1" }
variable "az" { default = "us-east-1a" }
variable "hosts" { description = "node hosts; 10 x 1000 nodes = 10k"; default = 10 }
variable "nodes_per_host" { default = 1000 }
variable "instance_type" { description = "Graviton: m7g.4xlarge is 16 vCPU / 64 GB"; default = "m7g.4xlarge" }
variable "coordinator_type" { default = "m7g.large" }
variable "spot" { description = "spot for hosts; do not use for 24h churn runs unless spot warnings are logged as departures"; default = false }
variable "binaries_url" { description = "URL of a tarball with topdisc-node, hostagent and testbed (linux/arm64)" }
