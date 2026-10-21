variable "hcloud_token" {
  sensitive = true
}
variable "location" { default = "fsn1" }
variable "subnet" {
  description = "the hosts' and coordinator's private addresses; the nodes live in 10.100.0.0/16 and up, one /16 per host"
  default     = "172.20.0.0/20"
}
variable "hosts" {
  description = "host servers; testbed fleet <scenario> prints the count (hetzner_machines)"
  default     = 1
}
variable "server_type" { default = "cpx51" }
variable "coordinator_type" { default = "cpx51" }
variable "ssh_public_key" {}
variable "admin_cidr" {
  description = "where SSH may come from"
  default     = "0.0.0.0/0"
}
