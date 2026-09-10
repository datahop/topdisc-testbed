# Packed fleet: N hosts, each running nodes_per_host node processes (netns per
# node), plus one coordinator. Private subnet only: no public IPv4 (billed per
# address), all traffic intra-VPC in one AZ.
terraform {
  required_providers { aws = { source = "hashicorp/aws", version = "~> 5.0" } }
}
provider "aws" { region = var.region }

data "aws_ami" "al2023_arm" {
  most_recent = true
  owners      = ["amazon"]
  filter { name = "name"; values = ["al2023-ami-*-arm64"] }
}

resource "aws_vpc" "tb" { cidr_block = "10.10.0.0/16"; enable_dns_hostnames = true; tags = { Name = "topdisc-testbed" } }
resource "aws_subnet" "nodes" { vpc_id = aws_vpc.tb.id; cidr_block = "10.10.0.0/18"; availability_zone = var.az }
resource "aws_security_group" "intra" {
  vpc_id = aws_vpc.tb.id
  ingress { from_port = 0; to_port = 0; protocol = "-1"; self = true }
  egress { from_port = 0; to_port = 0; protocol = "-1"; cidr_blocks = ["0.0.0.0/0"] }
}
# SSM for shell access without public IPs or SSH keys.
resource "aws_iam_role" "ssm" {
  name               = "topdisc-testbed-ssm"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_role_policy_attachment" "ssm" { role = aws_iam_role.ssm.name; policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore" }
resource "aws_iam_instance_profile" "ssm" { name = "topdisc-testbed-ssm"; role = aws_iam_role.ssm.name }
resource "aws_vpc_endpoint" "ssm" {
  for_each          = toset(["ssm", "ssmmessages", "ec2messages"])
  vpc_id            = aws_vpc.tb.id
  service_name      = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type = "Interface"
  subnet_ids        = [aws_subnet.nodes.id]
  security_group_ids = [aws_security_group.intra.id]
  private_dns_enabled = true
}

locals {
  cloud_init = templatefile("${path.module}/../../cloud-init.yaml.tftpl", { binaries_url = var.binaries_url, nodes_per_host = var.nodes_per_host })
}

resource "aws_instance" "coordinator" {
  ami = data.aws_ami.al2023_arm.id; instance_type = var.coordinator_type
  subnet_id = aws_subnet.nodes.id; vpc_security_group_ids = [aws_security_group.intra.id]
  iam_instance_profile = aws_iam_instance_profile.ssm.name
  user_data = local.cloud_init
  root_block_device { volume_size = 20; volume_type = "gp3" }
  tags = { Name = "topdisc-coordinator", role = "coordinator" }
}

resource "aws_instance" "host" {
  count = var.hosts
  ami = data.aws_ami.al2023_arm.id; instance_type = var.instance_type
  subnet_id = aws_subnet.nodes.id; vpc_security_group_ids = [aws_security_group.intra.id]
  iam_instance_profile = aws_iam_instance_profile.ssm.name
  user_data = local.cloud_init
  source_dest_check = false # netns subnets (10.1xx.0.0/16) are routed to the host
  root_block_device { volume_size = 8; volume_type = "gp3" }
  dynamic "instance_market_options" {
    for_each = var.spot ? [1] : []
    content { market_type = "spot"; spot_options { spot_instance_type = "one-time" } }
  }
  tags = { Name = "topdisc-host-${count.index}", role = "host", index = count.index }
}

# WAN emulation: host h owns 10.(100+h).0.0/16 for its node namespaces.
resource "aws_route" "host_subnet" {
  count                  = var.hosts
  route_table_id         = aws_vpc.tb.main_route_table_id
  destination_cidr_block = "10.${100 + count.index}.0.0/16"
  network_interface_id   = aws_instance.host[count.index].primary_network_interface_id
}
